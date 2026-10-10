package notify

import (
	"context"
	"strings"
	"time"
)

// SimulateInput describes a hypothetical alert for Simulate ("who would be notified for ...").
type SimulateInput struct {
	Type       string    `json:"type"`
	Severity   string    `json:"severity"`
	Host       string    `json:"host"`
	Repository string    `json:"repository"`
	Object     string    `json:"object"`
	Key        string    `json:"key"`
	At         time.Time `json:"at"` // zero = now
}

// SimulateChannel is one channel a matching rule would use.
type SimulateChannel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Kind    string `json:"kind"`
	Enabled bool   `json:"enabled"`
}

// SimulateRule is the verdict of one rule.
type SimulateRule struct {
	RuleID   string            `json:"rule_id"`
	RuleName string            `json:"rule_name"`
	Matched  bool              `json:"matched"`
	Reason   string            `json:"reason"` // why it does not match, or "" when it does
	Quiet    bool              `json:"quiet"`  // matched, but held by quiet hours at the given time
	Channels []SimulateChannel `json:"channels"`
}

// SimulateResult is the answer of Simulate.
type SimulateResult struct {
	At       time.Time      `json:"at"`
	Silenced []int64        `json:"silenced_by"` // active silences that would hold every message
	Rules    []SimulateRule `json:"rules"`
	// Deliver is true when at least one enabled channel would get a message right now.
	Deliver bool `json:"deliver"`
}

// Simulate evaluates every rule against a hypothetical alert with the same matching code the runner
// uses. It changes nothing.
func (e *Engine) Simulate(ctx context.Context, in SimulateInput) (*SimulateResult, error) {
	if in.Type == "" {
		return nil, bad("simulate: type required")
	}
	if in.Severity == "" {
		in.Severity = SeverityWarning
	}
	if !validSeverity(in.Severity) {
		return nil, bad("simulate: invalid severity %q", in.Severity)
	}
	at := in.At
	if at.IsZero() {
		at = e.now()
	}
	a := &Alert{Type: in.Type, Severity: in.Severity, Host: in.Host, Repository: in.Repository, Object: in.Object, Key: in.Key}
	if a.Key == "" {
		a.Key = in.Type + ":" + strings.Trim(in.Repository+"/"+in.Object, "/")
	}
	res := &SimulateResult{At: at, Silenced: []int64{}, Rules: []SimulateRule{}}
	sils, err := e.ListSilences(ctx, false)
	if err != nil {
		return nil, err
	}
	for i := range sils {
		s := sils[i]
		if !s.StartsAt.After(at) && s.Until.After(at) && s.matches(a) {
			res.Silenced = append(res.Silenced, s.ID)
		}
	}
	chans, err := e.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	byID := map[string]Channel{}
	for _, c := range chans {
		byID[c.ID] = c
	}
	rules, err := e.ListRules(ctx)
	if err != nil {
		return nil, err
	}
	for i := range rules {
		r := &rules[i]
		v := SimulateRule{RuleID: r.ID, RuleName: r.Name, Channels: []SimulateChannel{}}
		for _, id := range r.ChannelIDs {
			if c, ok := byID[id]; ok {
				v.Channels = append(v.Channels, SimulateChannel{ID: c.ID, Name: c.Name, Kind: c.Kind, Enabled: c.Enabled})
			}
		}
		switch {
		case !r.Enabled:
			v.Reason = "the rule is disabled"
		case r.Kind == RuleDigest:
			v.Reason = "a digest rule sends the daily summary, not single alerts"
		case sevRank(a.Severity) < sevRank(r.MinSeverity):
			v.Reason = "severity " + a.Severity + " is below the rule's minimum (" + r.MinSeverity + ")"
		case !r.matchesIgnoringSeverity(a):
			v.Reason = mismatch(r, a)
		default:
			v.Matched = true
			v.Quiet = r.quiet(at) && !(r.CriticalBypass && a.Severity == SeverityCritical)
			if !v.Quiet && len(res.Silenced) == 0 {
				for _, c := range v.Channels {
					if c.Enabled {
						res.Deliver = true
					}
				}
			}
		}
		res.Rules = append(res.Rules, v)
	}
	return res, nil
}

// mismatch names the first filter of r that a fails.
func mismatch(r *Rule, a *Alert) string {
	if len(r.EventTypes) > 0 {
		ok := false
		for _, t := range r.EventTypes {
			if glob(t)(a.Type) {
				ok = true
			}
		}
		if !ok {
			return "the alert type is not one of " + strings.Join(r.EventTypes, ", ")
		}
	}
	switch {
	case !glob(r.HostGlob)(a.Host):
		return "the host does not match " + r.HostGlob
	case !glob(r.RepoGlob)(a.Repository):
		return "the repository does not match " + r.RepoGlob
	case !glob(r.ObjectGlob)(a.Object):
		return "the object does not match " + r.ObjectGlob
	}
	return "no match"
}

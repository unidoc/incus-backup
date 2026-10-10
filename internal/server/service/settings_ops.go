package service

import (
	"context"
	"net/url"
)

// Operations settings (update notice). They are ordinary server settings (GET/PUT /settings).
func init() {
	// update_check: "on" asks the release feed once a day for a newer version; "" or "off" (default)
	// never contacts anything.
	settingValidators["update_check"] = func(v string) error {
		switch v {
		case "", "on", "off":
			return nil
		}
		return bad("update_check must be on or off")
	}
	// update_check_url: the release feed ("" = the project's GitHub releases/latest).
	settingValidators["update_check_url"] = func(v string) error {
		if v == "" {
			return nil
		}
		u, err := url.Parse(v)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
			return bad("update_check_url must be an http(s) URL")
		}
		return nil
	}
}

// UpdateCheckSettings returns whether update checks are on and the feed URL.
func (s *Service) UpdateCheckSettings(ctx context.Context) (bool, string) {
	return s.Setting(ctx, "update_check") == "on", s.Setting(ctx, "update_check_url")
}

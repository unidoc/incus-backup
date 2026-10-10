//go:build js && wasm

// Command ibwasm is the WebAssembly module of the browser library (web-crypto/). It exposes
// internal/webcore on the global object "ibwasm". Every function is synchronous, takes and returns
// plain values (strings, numbers, Uint8Array, JSON strings) and never returns key material:
// keys, catalogs, walks, hashes and sealed streams are integer handles.
//
// Calling convention: every function returns {ok: value} or {err: "message"}. A Go panic is
// recovered into {err} so hostile input can never kill the runtime.
package main

import (
	"encoding/json"
	"fmt"
	"syscall/js"

	"github.com/unidoc/incus-backup/internal/webcore"
)

var (
	core     = webcore.New()
	uint8Arr = js.Global().Get("Uint8Array")
	float64A = js.Global().Get("Float64Array")
)

func bytesArg(v js.Value) []byte {
	if v.IsUndefined() || v.IsNull() {
		return nil
	}
	if !v.InstanceOf(uint8Arr) {
		panic("expected a Uint8Array")
	}
	b := make([]byte, v.Get("byteLength").Int())
	js.CopyBytesToGo(b, v)
	return b
}

func bytesVal(b []byte) js.Value {
	a := uint8Arr.New(len(b))
	js.CopyBytesToJS(a, b)
	return a
}

func jsonVal(v any) js.Value {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return js.ValueOf(string(b))
}

func result(v any, err error) js.Value {
	if err != nil {
		return js.ValueOf(map[string]any{"err": err.Error()})
	}
	return js.ValueOf(map[string]any{"ok": v})
}

func fn(f func(args []js.Value) (any, error)) js.Func {
	return js.FuncOf(func(_ js.Value, args []js.Value) (ret any) {
		defer func() {
			if r := recover(); r != nil {
				ret = result(nil, fmt.Errorf("internal error: %v", r))
			}
		}()
		v, err := f(args)
		return result(v, err)
	})
}

func main() {
	api := map[string]js.Func{
		"loadRecoveryKey": fn(func(a []js.Value) (any, error) {
			h, d, err := core.LoadRecoveryKey(a[0].String())
			return map[string]any{"key": h, "domainId": d}, err
		}),
		"loadEscrow": fn(func(a []js.Value) (any, error) {
			h, d, err := core.LoadEscrow(bytesArg(a[0]), a[1].String())
			return map[string]any{"key": h, "domainId": d}, err
		}),
		"dropKey": fn(func(a []js.Value) (any, error) { core.DropKey(a[0].Int()); return true, nil }),
		"zeroize": fn(func(a []js.Value) (any, error) { core.Zeroize(); return true, nil }),
		"groupId": fn(func(a []js.Value) (any, error) {
			return core.GroupID(a[0].Int(), a[1].String(), a[2].String(), a[3].String())
		}),
		"openSnapshot": fn(func(a []js.Value) (any, error) {
			var expect *webcore.Identity
			if len(a) > 5 && a[5].Type() == js.TypeString {
				expect = &webcore.Identity{}
				if err := json.Unmarshal([]byte(a[5].String()), expect); err != nil {
					return nil, fmt.Errorf("expected identity: %v", err)
				}
			}
			si, err := core.OpenSnapshot(a[0].Int(), a[1].String(), a[2].String(), bytesArg(a[3]), bytesArg(a[4]), expect)
			if err != nil {
				return nil, err
			}
			ends := float64A.New(len(si.Ends))
			for i, e := range si.Ends {
				ends.SetIndex(i, float64(e)) // exact below 2^53 bytes (8 PiB)
			}
			return map[string]any{"info": jsonVal(si), "ends": ends}, nil
		}),
		"decodeChunk": fn(func(a []js.Value) (any, error) {
			pt, err := core.DecodeChunk(a[0].Int(), bytesArg(a[1]), a[2].String())
			if err != nil {
				return nil, err
			}
			return bytesVal(pt), nil
		}),
		"catalogOpen": fn(func(a []js.Value) (any, error) {
			h, n, st, err := core.CatalogOpen(bytesArg(a[0]))
			return map[string]any{"cat": h, "count": n, "stats": jsonVal(st)}, err
		}),
		"catalogClose": fn(func(a []js.Value) (any, error) { core.CatalogClose(a[0].Int()); return true, nil }),
		"list": fn(func(a []js.Value) (any, error) {
			l, err := core.List(a[0].Int(), a[1].String())
			return jsonVal(l), err
		}),
		"stat": fn(func(a []js.Value) (any, error) {
			e, err := core.Stat(a[0].Int(), a[1].String())
			return jsonVal(e), err
		}),
		"find": fn(func(a []js.Value) (any, error) {
			l, err := core.Find(a[0].Int(), a[1].String(), a[2].String())
			return jsonVal(l), err
		}),
		"subtree": fn(func(a []js.Value) (any, error) {
			n, b, err := core.Subtree(a[0].Int(), a[1].String())
			return map[string]any{"entries": n, "bytes": float64(b)}, err
		}),
		"resolveFile": fn(func(a []js.Value) (any, error) {
			r, err := core.ResolveFile(a[0].Int(), a[1].String())
			return jsonVal(r), err
		}),
		"walkOpen": fn(func(a []js.Value) (any, error) { return core.WalkOpen(a[0].Int(), a[1].String()) }),
		"walkNext": fn(func(a []js.Value) (any, error) {
			l, done, err := core.WalkNext(a[0].Int(), a[1].Int())
			return map[string]any{"entries": jsonVal(l), "done": done}, err
		}),
		"walkClose": fn(func(a []js.Value) (any, error) { core.WalkClose(a[0].Int()); return true, nil }),
		"hashNew":   fn(func(a []js.Value) (any, error) { return core.HashNew(), nil }),
		"hashUpdate": fn(func(a []js.Value) (any, error) {
			return true, core.HashUpdate(a[0].Int(), bytesArg(a[1]))
		}),
		"hashZeros": fn(func(a []js.Value) (any, error) {
			return true, core.HashZeros(a[0].Int(), uint64(a[1].Float()))
		}),
		"hashSum": fn(func(a []js.Value) (any, error) { return core.HashSum(a[0].Int()) }),
		// sealed deliveries: the one-time private key never leaves the module (a handle)
		"sealNew": fn(func(a []js.Value) (any, error) {
			h, pub, err := core.SealNew()
			if err != nil {
				return nil, err
			}
			return map[string]any{"seal": h, "publicKey": bytesVal(pub)}, nil
		}),
		"sealImport": fn(func(a []js.Value) (any, error) {
			h, pub, err := core.SealImport(bytesArg(a[0]))
			if err != nil {
				return nil, err
			}
			return map[string]any{"seal": h, "publicKey": bytesVal(pub)}, nil
		}),
		"sealDrop": fn(func(a []js.Value) (any, error) { core.SealDrop(a[0].Int()); return true, nil }),
		"requestDigest": fn(func(a []js.Value) (any, error) {
			d, err := webcore.RequestDigest(a[0].String(), []byte(a[1].String()))
			if err != nil {
				return nil, err
			}
			return bytesVal(d), nil
		}),
		"unsealOpen": fn(func(a []js.Value) (any, error) {
			return core.UnsealOpen(a[0].Int(), a[1].Int(), a[2].String(), bytesArg(a[3]))
		}),
		"unsealPush": fn(func(a []js.Value) (any, error) {
			pt, err := core.UnsealPush(a[0].Int(), bytesArg(a[1]))
			if err != nil {
				return nil, err
			}
			return bytesVal(pt), nil
		}),
		"unsealEnd":   fn(func(a []js.Value) (any, error) { return true, core.UnsealEnd(a[0].Int()) }),
		"unsealClose": fn(func(a []js.Value) (any, error) { core.UnsealClose(a[0].Int()); return true, nil }),
	}
	obj := js.Global().Get("Object").New()
	for k, f := range api {
		obj.Set(k, f)
	}
	obj.Set("version", 2)
	js.Global().Set("ibwasm", obj)
	if cb := js.Global().Get("__ibwasmReady"); cb.Type() == js.TypeFunction {
		cb.Invoke()
	}
	select {}
}

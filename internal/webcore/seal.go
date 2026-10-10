package webcore

import (
	"crypto/ecdh"
	"crypto/rand"
	"encoding/json"
	"errors"

	"github.com/unidoc/incus-backup/internal/format"
	"github.com/unidoc/incus-backup/internal/jobspec"
)

// ---- sealed deliveries (format IBS2) ----------------------------------------------------

// SealNew creates a one-time X25519 key pair for a sealed delivery. The private key stays inside
// the Core (a handle is returned); only the public key leaves it.
func (c *Core) SealNew() (int, []byte, error) {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return 0, nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.id()
	c.seals[h] = k
	return h, k.PublicKey().Bytes(), nil
}

// SealImport keeps an existing X25519 private key under a handle (interoperability and tests;
// the bytes are wiped here, the caller should wipe its copy).
func (c *Core) SealImport(priv []byte) (int, []byte, error) {
	defer wipe(priv)
	k, err := ecdh.X25519().NewPrivateKey(priv)
	if err != nil {
		return 0, nil, errors.New("the sealed-delivery private key is not a valid X25519 key")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.id()
	c.seals[h] = k
	return h, k.PublicKey().Bytes(), nil
}

// SealDrop forgets a one-time key.
func (c *Core) SealDrop(h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.seals, h)
}

// RequestDigest is jobspec.RequestDigest: the browser computes it from the parameters IT sent, so
// a delivery for parameters the server changed does not authenticate.
func RequestDigest(typ string, params []byte) ([]byte, error) {
	if !json.Valid(params) {
		return nil, errors.New("job parameters are not JSON")
	}
	return jobspec.RequestDigest(typ, params)
}

// unsealer adapts format.SealOpener to pushed bytes. It holds back the plaintext of the newest
// data frame until the next frame or the trailer authenticated, so the last piece of a delivery is
// only ever released after the whole stream verified.
type unsealer struct {
	o    *format.SealOpener
	buf  []byte
	hdr  bool
	held []byte
	done bool
	err  error
}

func (u *unsealer) close() {
	wipe(u.buf)
	wipe(u.held)
	u.buf, u.held = nil, nil
}

// UnsealOpen starts verifying and decrypting a sealed delivery addressed to the one-time key seal,
// authenticated with the repository key key for job jobID and request digest digest.
func (c *Core) UnsealOpen(key, seal int, jobID string, digest []byte) (int, error) {
	k, err := c.key(key)
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	sk, ok := c.seals[seal]
	c.mu.Unlock()
	if !ok {
		return 0, errors.New("the sealed-delivery key was discarded")
	}
	o, err := format.NewSealOpenerKey(k, sk, format.SealBinding{JobID: jobID, Request: digest})
	if err != nil {
		return 0, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	h := c.id()
	c.unseals[h] = &unsealer{o: o}
	return h, nil
}

func (c *Core) unsealer(h int) (*unsealer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	u, ok := c.unseals[h]
	if !ok {
		return nil, errors.New("unknown sealed stream")
	}
	return u, nil
}

// UnsealPush adds sealed bytes and returns the plaintext released by them: every verified data
// frame except the newest, which follows with the next frame (or the trailer).
func (c *Core) UnsealPush(h int, data []byte) ([]byte, error) {
	u, err := c.unsealer(h)
	if err != nil {
		return nil, err
	}
	if u.err != nil {
		return nil, u.err
	}
	fail := func(err error) ([]byte, error) { u.err = err; u.close(); return nil, err }
	if u.done && len(data) > 0 {
		return fail(errors.New("sealed stream: data after the final frame"))
	}
	u.buf = append(u.buf, data...)
	var plain []byte
	for {
		if !u.hdr {
			if len(u.buf) >= 4 && string(u.buf[:4]) == "IBS1" {
				return fail(format.ErrSealV1)
			}
			if len(u.buf) < format.SealHeaderSize {
				return plain, nil
			}
			if err := u.o.Header(u.buf[:format.SealHeaderSize]); err != nil {
				return fail(err)
			}
			u.buf = u.buf[format.SealHeaderSize:]
			u.hdr = true
			continue
		}
		if u.done {
			if len(u.buf) > 0 {
				return fail(errors.New("sealed stream: data after the final frame"))
			}
			return plain, nil
		}
		if len(u.buf) < 4 {
			return plain, nil
		}
		n, err := format.FrameLen(u.buf[:4])
		if err != nil {
			return fail(err)
		}
		if len(u.buf) < 4+n {
			return plain, nil
		}
		pt, trailer, err := u.o.Frame(u.buf[4 : 4+n])
		if err != nil {
			return fail(err)
		}
		u.buf = u.buf[4+n:]
		plain = append(plain, u.held...)
		u.held = pt
		if trailer {
			u.done = true
			plain = append(plain, u.held...)
			u.held = nil
		}
	}
}

// UnsealEnd checks that the stream ended with its authenticated trailer and forgets it.
func (c *Core) UnsealEnd(h int) error {
	u, err := c.unsealer(h)
	if err != nil {
		return err
	}
	defer c.UnsealClose(h)
	if u.err != nil {
		return u.err
	}
	if !u.hdr {
		if len(u.buf) == 0 {
			return errors.New("sealed stream is empty")
		}
		return errors.New("sealed stream is truncated (incomplete header)")
	}
	if !u.done {
		return errors.New("sealed stream is truncated (no final frame)")
	}
	return u.o.End()
}

// UnsealClose forgets a stream and wipes what it buffered.
func (c *Core) UnsealClose(h int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if u, ok := c.unseals[h]; ok {
		u.close()
		delete(c.unseals, h)
	}
}

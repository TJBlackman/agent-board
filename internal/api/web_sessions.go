package api

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"sync"
	"time"
)

const (
	webSessionMax     = 32
	webPreSessionMax  = 128
	webSessionLife    = 12 * time.Hour
	webSessionIdle    = time.Hour
	webPreSessionLife = 10 * time.Minute
)

var errWebCapacity = errors.New("browser session capacity reached")

type webSession struct {
	Principal, Instance, Generation, CSRF string
	Expires, LastSeen                     time.Time
}

type webSessions struct {
	mu        sync.Mutex
	auth, pre map[[32]byte]webSession
	now       func() time.Time
}

func newWebSessions() *webSessions {
	return &webSessions{auth: make(map[[32]byte]webSession), pre: make(map[[32]byte]webSession), now: time.Now}
}

func webRandom() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

func csrfEqual(a, b string) bool {
	return len(a) == 43 && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func (s *webSessions) create(pre bool, v webSession) (string, webSession, error) {
	id, err := webRandom()
	if err != nil {
		return "", v, err
	}
	v.CSRF, err = webRandom()
	if err != nil {
		return "", v, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	m, max, life := s.auth, webSessionMax, webSessionLife
	if pre {
		m, max, life = s.pre, webPreSessionMax, webPreSessionLife
	}
	for k, old := range m {
		if !now.Before(old.Expires) || !pre && now.Sub(old.LastSeen) >= webSessionIdle {
			delete(m, k)
		}
	}
	if len(m) >= max {
		return "", v, errWebCapacity
	}
	v.Expires, v.LastSeen = now.Add(life), now
	m[sha256.Sum256([]byte(id))] = v
	return id, v, nil
}

func (s *webSessions) get(pre bool, id string) (webSession, bool) {
	if len(id) != 43 {
		return webSession{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.auth
	if pre {
		m = s.pre
	}
	k := sha256.Sum256([]byte(id))
	v, ok := m[k]
	now := s.now()
	if !ok {
		return v, false
	}
	if !now.Before(v.Expires) || !pre && now.Sub(v.LastSeen) >= webSessionIdle {
		delete(m, k)
		return webSession{}, false
	}
	v.LastSeen = now
	m[k] = v
	return v, true
}

func (s *webSessions) remove(pre bool, id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.auth
	if pre {
		m = s.pre
	}
	delete(m, sha256.Sum256([]byte(id)))
}

// consumePre validates and consumes a login form atomically, including on failed logins.
func (s *webSessions) consumePre(id, csrf string) bool {
	if len(id) != 43 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	k := sha256.Sum256([]byte(id))
	v, ok := s.pre[k]
	if !ok || !s.now().Before(v.Expires) || !csrfEqual(v.CSRF, csrf) {
		return false
	}
	delete(s.pre, k)
	return true
}

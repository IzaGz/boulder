package wfe

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"fmt"
	mrand "math/rand"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/letsencrypt/boulder/Godeps/_workspace/src/github.com/letsencrypt/go-jose"
	"github.com/letsencrypt/boulder/cmd/load-generator/latency"

	"github.com/letsencrypt/boulder/core"
)

type registration struct {
	key    *rsa.PrivateKey
	signer jose.Signer
	iMu    *sync.RWMutex
	auths  []core.Authorization
	certs  []string
}

type State struct {
	rMu     *sync.RWMutex
	regs    []*registration
	maxRegs int
	client  *http.Client
	apiBase string

	nMu       *sync.Mutex
	noncePool []string

	throughput int64

	hoMu              *sync.RWMutex
	httpOneChallenges map[string]string
	httpOnePort       int

	certKey    *rsa.PrivateKey
	domainBase string

	callLatency *latency.Map

	runtime time.Duration
}

func New(httpOnePort int, apiBase string, rate int, maxRegs int, keySize int, domainBase string, runtime time.Duration) (*State, error) {
	certKey, err := rsa.GenerateKey(rand.Reader, keySize)
	if err != nil {
		return nil, err
	}
	return &State{
		rMu:               new(sync.RWMutex),
		nMu:               new(sync.Mutex),
		hoMu:              new(sync.RWMutex),
		httpOneChallenges: make(map[string]string),
		httpOnePort:       httpOnePort,
		client:            new(http.Client),
		apiBase:           apiBase,
		throughput:        int64(rate),
		maxRegs:           maxRegs,
		certKey:           certKey,
		domainBase:        domainBase,
		callLatency:       latency.New(0, (time.Second * 5).Nanoseconds(), 5),
		runtime:           runtime,
	}, nil
}

func (s *State) Run() {
	// Run http-0 challenge server
	go s.httpOneServer()

	// Run sending loop
	stop := make(chan bool, 1)
	go func() {
		select {
		case <-stop:
			return
		default:
			for {
				go s.sendCall()
				time.Sleep(time.Duration(time.Second.Nanoseconds() / atomic.LoadInt64(&s.throughput)))
			}
		}
	}()

	time.Sleep(s.runtime)
	stop <- true
}

func (s *State) Dump(jsonPath string) {
	fmt.Println("WFE latency histograms")
	fmt.Printf("######################\n%s", s.callLatency)

	if jsonPath != "" {
		// Something something
	}
}

// HTTP utils

func (s *State) post(endpoint string, payload []byte) (*http.Response, error) {
	resp, err := s.client.Post(
		endpoint,
		"application/json",
		bytes.NewBuffer(payload),
	)
	if resp != nil {
		if newNonce := resp.Header.Get("Replay-Nonce"); newNonce != "" {
			s.addNonce(newNonce)
		}
	}
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// Nonce utils, these methods are used to generate/store/retrieve the nonces
// required for the required form of JWS

func (s *State) signWithNonce(payload []byte, signer jose.Signer) ([]byte, error) {
	nonce, err := s.getNonce()
	if err != nil {
		return nil, err
	}
	jws, err := signer.Sign(payload, nonce)
	if err != nil {
		return nil, err
	}
	return json.Marshal(jws)
}

func (s *State) getNonce() (string, error) {
	s.nMu.Lock()
	defer s.nMu.Unlock()
	if len(s.noncePool) == 0 {
		started := time.Now()
		resp, err := s.client.Head(fmt.Sprintf("%s/directory", s.apiBase))
		s.callLatency.Add("HEAD /directory", time.Since(started))
		if err != nil {
			return "", err
		}
		if nonce := resp.Header.Get("Replay-Nonce"); nonce != "" {
			return nonce, nil
		}
		return "", fmt.Errorf("Nonce header not supplied!")
	}
	nonce := s.noncePool[0]
	s.noncePool = s.noncePool[1:]
	return nonce, nil
}

func (s *State) addNonce(nonce string) {
	s.nMu.Lock()
	defer s.nMu.Unlock()
	s.noncePool = append(s.noncePool, nonce)
}

// Reg object utils, used to add and randomly retrieve registration objects

func (s *State) addReg(reg *registration) {
	s.rMu.Lock()
	defer s.rMu.Unlock()
	s.regs = append(s.regs, reg)
}

func (s *State) getRandReg() (*registration, bool) {
	regsLength := len(s.regs)
	if regsLength == 0 {
		return nil, false
	}
	return s.regs[mrand.Intn(regsLength)], true
}

func (s *State) getReg() (*registration, bool) {
	s.rMu.RLock()
	defer s.rMu.RUnlock()
	return s.getRandReg()
}

// Call sender, it sends the calls!

func (s *State) sendCall() {
	actions := []func(*registration){}
	s.rMu.RLock()
	if len(s.regs) < s.maxRegs || s.maxRegs == 0 {
		actions = append(actions, s.newRegistration)
	}
	s.rMu.RUnlock()

	reg, found := s.getReg()
	if found {
		actions = append(actions, s.newAuthorization)
		reg.iMu.RLock()
		if len(reg.auths) > 0 {
			actions = append(actions, s.newCertificate)
		}
		if len(reg.certs) > 2 { // XXX: makes life more interesting
			actions = append(actions, s.revokeCertificate)
		}
		reg.iMu.RUnlock()
	}

	if len(actions) > 0 {
		actions[mrand.Intn(len(actions))](reg)
	} else {
		fmt.Println("wat")
	}
}
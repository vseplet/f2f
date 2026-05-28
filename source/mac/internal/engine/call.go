//go:build darwin

package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/vseplet/f2f/source/mac/internal/sfu"
)

type CallState struct {
	CallID       string                `json:"call_id"`
	SFUHost      string                `json:"sfu_host"`
	Participants []sfu.ParticipantInfo `json:"participants"`
	StartedAt    time.Time             `json:"started_at"`
	Remote       bool                  `json:"remote"`
}

type callCtx struct {
	state CallState
	sfu   *sfu.SFU
}

func (e *Engine) loadCall() *callCtx {
	v := e.call.Load()
	if v == nil {
		return nil
	}
	cc, _ := v.(*callCtx)
	return cc
}

func (e *Engine) loadRemoteCalls() []CallState {
	v := e.remoteCalls.Load()
	if v == nil {
		return nil
	}
	p, _ := v.(*[]CallState)
	if p == nil {
		return nil
	}
	return *p
}

func (e *Engine) clearCall() {
	e.call.Store((*callCtx)(nil))
}

func (e *Engine) storeRemoteCalls(calls []CallState) {
	e.remoteCalls.Store(&calls)
}

func (e *Engine) JoinedSFUHost() string {
	v := e.joinedSFUHost.Load()
	if v == nil {
		return ""
	}
	p, _ := v.(*string)
	if p == nil {
		return ""
	}
	return *p
}

func (e *Engine) SetJoinedSFUHost(host string) {
	e.joinedSFUHost.Store(&host)
}

func (e *Engine) ClearJoinedSFUHost() {
	e.joinedSFUHost.Store((*string)(nil))
}

// CallState returns the local call if we're the SFU host.
func (e *Engine) CallState() *CallState {
	if cc := e.loadCall(); cc != nil {
		st := cc.state
		st.Participants = cc.sfu.Participants()
		st.Remote = false
		return &st
	}
	return nil
}

// RemoteCalls returns active calls discovered on remote peers.
func (e *Engine) RemoteCalls() []CallState {
	return e.loadRemoteCalls()
}

// AllCalls returns local + remote calls for the UI.
func (e *Engine) AllCalls() []CallState {
	var out []CallState
	if cs := e.CallState(); cs != nil {
		out = append(out, *cs)
	}
	for _, rc := range e.loadRemoteCalls() {
		out = append(out, rc)
	}
	return out
}

func (e *Engine) CallSFU() *sfu.SFU {
	if cc := e.loadCall(); cc != nil {
		return cc.sfu
	}
	return nil
}

func (e *Engine) CreateCall() (*CallState, error) {
	if cc := e.loadCall(); cc != nil {
		return nil, fmt.Errorf("call already active")
	}

	st := e.Status()
	if !st.Running {
		return nil, fmt.Errorf("engine not running")
	}

	// Can't be joined to a remote SFU while hosting our own.
	e.ClearJoinedSFUHost()

	sfuInst := sfu.New(st.LocalIP, st.UtunName, func(to string, msg []byte) {
		e.deliverSFUSignal(to, msg)
	})

	cc := &callCtx{
		state: CallState{
			CallID:    fmt.Sprintf("%d", time.Now().UnixNano()),
			SFUHost:   st.LocalIP,
			StartedAt: time.Now(),
		},
		sfu: sfuInst,
	}
	e.call.Store(cc)

	if _, err := sfuInst.AddParticipant(st.LocalIP, st.CampName); err != nil {
		sfuInst.Close()
		e.clearCall()
		return nil, fmt.Errorf("add self to sfu: %w", err)
	}

	log.Printf("call: created %s, sfu host %s", cc.state.CallID, st.LocalIP)
	return e.CallState(), nil
}

func (e *Engine) JoinCall(tunnelIP, name string) error {
	cc := e.loadCall()
	if cc == nil {
		return fmt.Errorf("no active call on this host")
	}
	_, err := cc.sfu.AddParticipant(tunnelIP, name)
	return err
}

func (e *Engine) LeaveCall(tunnelIP string) {
	cc := e.loadCall()
	if cc == nil {
		return
	}

	if tunnelIP == cc.state.SFUHost {
		cc.sfu.Close()
		e.clearCall()
		log.Printf("call: ended (host left)")
		return
	}

	cc.sfu.RemoveParticipant(tunnelIP)
	if len(cc.sfu.Participants()) == 0 {
		cc.sfu.Close()
		e.clearCall()
		log.Printf("call: ended (last participant left)")
	}
}

func (e *Engine) EndCall() {
	cc := e.loadCall()
	if cc == nil {
		return
	}
	cc.sfu.Close()
	e.clearCall()
	log.Printf("call: ended")
}

func (e *Engine) HandleCallSignal(fromTunnelIP string, body []byte) ([]byte, error) {
	cc := e.loadCall()
	if cc == nil {
		return nil, fmt.Errorf("no active call")
	}
	return cc.sfu.HandleSignal(fromTunnelIP, body)
}

func (e *Engine) deliverSFUSignal(to string, msg []byte) {
	st := e.Status()
	if to == st.LocalIP && e.OnLocalSFUSignal != nil {
		e.OnLocalSFUSignal(msg)
		return
	}
	port := e.tunnelHTTPPort
	if port == "" {
		port = "2202"
	}
	url := "http://" + to + ":" + port + "/api/call/signal"
	go func() {
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Post(url, "application/json", bytes.NewReader(msg))
		if err != nil {
			log.Printf("call: deliver signal to %s: %v", to, err)
			return
		}
		resp.Body.Close()
	}()
}

func (e *Engine) callPollLoop(ctx context.Context) {
	defer e.workers.Done()
	ticker := time.NewTicker(3 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		e.pollRemoteCalls(ctx)
	}
}

func (e *Engine) pollRemoteCalls(ctx context.Context) {
	type target struct {
		host string
		name string
	}
	var targets []target
	e.mu.Lock()
	for _, p := range e.peers {
		if !p.IsOnline() {
			continue
		}
		h := e.peerHTTPHostLocked(p)
		if h != "" {
			targets = append(targets, target{host: h, name: p.Name})
		}
	}
	port := domainPollPort(e)
	e.mu.Unlock()
	if port == "" {
		return
	}

	client := &http.Client{Timeout: 3 * time.Second}
	var found []CallState
	for _, t := range targets {
		url := "http://" + net.JoinHostPort(t.host, port) + "/api/call/state"
		req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
		if err != nil {
			continue
		}
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		var cs CallState
		if err := json.NewDecoder(resp.Body).Decode(&cs); err != nil {
			resp.Body.Close()
			continue
		}
		resp.Body.Close()

		if cs.CallID != "" {
			cs.Remote = true
			found = append(found, cs)
		}
	}
	e.storeRemoteCalls(found)
}

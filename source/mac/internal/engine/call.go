//go:build darwin

package engine

import (
	"bytes"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/vseplet/f2f/source/mac/internal/sfu"
)

type CallState struct {
	CallID       string                `json:"call_id"`
	SFUHost      string                `json:"sfu_host"`
	Participants []sfu.ParticipantInfo `json:"participants"`
	StartedAt    time.Time             `json:"started_at"`
}

type callCtx struct {
	state CallState
	sfu   *sfu.SFU
}

func (e *Engine) CallState() *CallState {
	v := e.call.Load()
	if v == nil {
		return nil
	}
	cc := v.(*callCtx)
	st := cc.state
	st.Participants = cc.sfu.Participants()
	return &st
}

func (e *Engine) CallSFU() *sfu.SFU {
	v := e.call.Load()
	if v == nil {
		return nil
	}
	return v.(*callCtx).sfu
}

func (e *Engine) CreateCall() (*CallState, error) {
	if e.call.Load() != nil {
		return nil, fmt.Errorf("call already active")
	}

	st := e.Status()
	if !st.Running {
		return nil, fmt.Errorf("engine not running")
	}

	sfuInst := sfu.New(func(to string, msg []byte) {
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
		e.call.Store((*callCtx)(nil))
		return nil, fmt.Errorf("add self to sfu: %w", err)
	}

	log.Printf("call: created %s, sfu host %s", cc.state.CallID, st.LocalIP)
	return e.CallState(), nil
}

func (e *Engine) JoinCall(tunnelIP, name string) error {
	v := e.call.Load()
	if v == nil {
		return fmt.Errorf("no active call")
	}
	_, err := v.(*callCtx).sfu.AddParticipant(tunnelIP, name)
	return err
}

func (e *Engine) LeaveCall(tunnelIP string) {
	v := e.call.Load()
	if v == nil {
		return
	}
	cc := v.(*callCtx)
	cc.sfu.RemoveParticipant(tunnelIP)

	if len(cc.sfu.Participants()) == 0 {
		cc.sfu.Close()
		e.call.Store((*callCtx)(nil))
		log.Printf("call: ended (last participant left)")
	}
}

func (e *Engine) EndCall() {
	v := e.call.Load()
	if v == nil {
		return
	}
	v.(*callCtx).sfu.Close()
	e.call.Store((*callCtx)(nil))
	log.Printf("call: ended")
}

func (e *Engine) HandleCallSignal(fromTunnelIP string, body []byte) ([]byte, error) {
	v := e.call.Load()
	if v == nil {
		return nil, fmt.Errorf("no active call")
	}
	return v.(*callCtx).sfu.HandleSignal(fromTunnelIP, body)
}

func (e *Engine) deliverSFUSignal(to string, msg []byte) {
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

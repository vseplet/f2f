package main

import (
	"context"

	"github.com/vseplet/f2f/source/desktop/internal/lite"
	"github.com/wailsapp/wails/v2/pkg/runtime"
)

// App is the Wails bound type. Methods on this type are auto-exposed
// to JavaScript via wails bindings (see frontend/wailsjs/go/main/App).
type App struct {
	ctx    context.Context
	client *lite.Client
}

func NewApp() *App {
	return &App{client: lite.New()}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	// Forward signal-frames from the lite client to the UI via a
	// Wails event. JS subscribes with EventsOn("signal", ...).
	a.client.OnSignal = func(from string, body []byte) {
		runtime.EventsEmit(a.ctx, "signal", map[string]any{
			"from": from,
			"body": string(body),
		})
	}
}

// StartReq is what the UI sends from the camp tab.
type StartReq struct {
	Name     string `json:"name"`
	ID       string `json:"id"`
	CampURL  string `json:"camp_url"`
	StunAddr string `json:"stun_addr"`
}

// Start joins the camp. Defaults match the fly-hosted camp server.
func (a *App) Start(req StartReq) error {
	if req.CampURL == "" {
		req.CampURL = "wss://f2f-camp.fly.dev/ws"
	}
	if req.StunAddr == "" {
		req.StunAddr = "f2f-camp.fly.dev:3478"
	}
	return a.client.Start(lite.Config{
		Name:     req.Name,
		ID:       req.ID,
		CampURL:  req.CampURL,
		StunAddr: req.StunAddr,
	})
}

func (a *App) Stop() error         { return a.client.Stop() }
func (a *App) Status() lite.Status { return a.client.Status() }

// SendSignal delivers an opaque payload to the peer at toTunnelIP via
// the lite client's hole-punched UDP socket. UI uses this for WebRTC
// signalling (SDP offer/answer, ICE candidates) and for any other
// ad-hoc small messages.
func (a *App) SendSignal(toTunnelIP, body string) error {
	return a.client.SendSignal(toTunnelIP, []byte(body))
}

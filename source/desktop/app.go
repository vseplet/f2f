package main

import (
	"context"
	"encoding/base64"
	"fmt"

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

// ---- drop tab bindings ----

// MyFiles returns the seeds we're currently sharing.
func (a *App) MyFiles() []lite.SeededFile { return a.client.MyFiles() }

// AddMyFileFromPath copies the file at srcPath into the shared
// directory and starts seeding it. Used by "open file" dialog UX.
func (a *App) AddMyFileFromPath(srcPath string) (*lite.SeededFile, error) {
	return a.client.AddSeedFromPath(srcPath)
}

// AddMyFileBytes is the path the drag-and-drop UI uses: JS reads the
// dropped File, base64-encodes the bytes (necessary to cross the Wails
// bridge), backend decodes and writes to shared/.
func (a *App) AddMyFileBytes(name string, base64Body string) (*lite.SeededFile, error) {
	data, err := base64.StdEncoding.DecodeString(base64Body)
	if err != nil {
		return nil, fmt.Errorf("decode base64: %w", err)
	}
	return a.client.AddSeedBytes(name, data)
}

// RemoveMyFile drops a seed by info_hash. File on disk is kept.
func (a *App) RemoveMyFile(infoHash string) error {
	return a.client.RemoveSeed(infoHash)
}

// Library returns every file every known peer has broadcast.
func (a *App) Library() []lite.PeerLibraryEntry { return a.client.Library() }

// StartDownload begins pulling a torrent and persists it. peer is the
// peer's UDP endpoint we should hand to anacrolix as a candidate.
func (a *App) StartDownload(magnet, peerAddr string) (*lite.DownloadInfo, error) {
	peers := []string{}
	if peerAddr != "" {
		peers = append(peers, peerAddr)
	}
	return a.client.AddDownload(magnet, peers)
}

// Downloads returns the live list of in-flight + completed transfers.
func (a *App) Downloads() []lite.DownloadInfo { return a.client.ListDownloads() }

// Reveal opens Finder with the given file selected.
func (a *App) Reveal(path string) error { return a.client.RevealFile(path) }

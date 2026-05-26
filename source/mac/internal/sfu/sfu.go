//go:build darwin

package sfu

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/interceptor/pkg/nack"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v4"
)

type publishedTrack struct {
	local       *webrtc.TrackLocalStaticRTP
	publisherPC *webrtc.PeerConnection
	remoteSSRC  webrtc.SSRC
}

type Participant struct {
	TunnelIP string
	Name     string
	PC       *webrtc.PeerConnection
	DC       *webrtc.DataChannel // chat relay
	mu       sync.Mutex
	tracks   map[string]*publishedTrack
}

type ParticipantInfo struct {
	TunnelIP string `json:"tunnel_ip"`
	Name     string `json:"name"`
}

type SFU struct {
	mu           sync.Mutex
	localAPI     *webrtc.API // for local participant (all interfaces)
	tunnelAPI    *webrtc.API // for remote participants (utun only)
	localIP      string
	participants map[string]*Participant
	onSignal     func(to string, msg []byte)
	renegTimers  map[string]*time.Timer
}

func New(localIP, tunIface string, onSignal func(to string, msg []byte)) *SFU {
	buildAPI := func(se webrtc.SettingEngine) *webrtc.API {
		m := &webrtc.MediaEngine{}
		if err := m.RegisterDefaultCodecs(); err != nil {
			log.Printf("sfu: register codecs: %v", err)
		}
		i := &interceptor.Registry{}
		responder, _ := nack.NewResponderInterceptor()
		i.Add(responder)
		generator, _ := nack.NewGeneratorInterceptor()
		i.Add(generator)
		if err := webrtc.RegisterDefaultInterceptors(m, i); err != nil {
			log.Printf("sfu: register interceptors: %v", err)
		}
		return webrtc.NewAPI(
			webrtc.WithMediaEngine(m),
			webrtc.WithInterceptorRegistry(i),
			webrtc.WithSettingEngine(se),
		)
	}

	localAPI := buildAPI(webrtc.SettingEngine{})

	tunSE := webrtc.SettingEngine{}
	if tunIface != "" {
		tunSE.SetInterfaceFilter(func(iface string) bool {
			return iface == tunIface
		})
	}
	tunnelAPI := buildAPI(tunSE)

	return &SFU{
		localAPI:     localAPI,
		tunnelAPI:    tunnelAPI,
		localIP:      localIP,
		participants: make(map[string]*Participant),
		onSignal:     onSignal,
		renegTimers:  make(map[string]*time.Timer),
	}
}

func (s *SFU) AddParticipant(tunnelIP, name string) (*Participant, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if old, ok := s.participants[tunnelIP]; ok {
		// Stale participant (e.g., browser page reload). Clean up before re-adding.
		old.PC.OnConnectionStateChange(func(webrtc.PeerConnectionState) {})
		old.mu.Lock()
		for _, pt := range old.tracks {
			for _, other := range s.participants {
				if other.TunnelIP == tunnelIP {
					continue
				}
				for _, sender := range other.PC.GetSenders() {
					if sender.Track() == pt.local {
						_ = other.PC.RemoveTrack(sender)
					}
				}
			}
		}
		old.mu.Unlock()
		delete(s.participants, tunnelIP)
		go old.PC.Close()
		log.Printf("sfu: replacing stale participant %s (%s)", name, tunnelIP)
	}

	api := s.tunnelAPI
	if tunnelIP == s.localIP {
		api = s.localAPI
	}
	pc, err := api.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		return nil, fmt.Errorf("new peer connection: %w", err)
	}

	p := &Participant{
		TunnelIP: tunnelIP,
		Name:     name,
		PC:       pc,
		tracks:   make(map[string]*publishedTrack),
	}

	// Add existing tracks from other participants and set up RTCP forwarding.
	for _, other := range s.participants {
		other.mu.Lock()
		for _, pt := range other.tracks {
			rtpSender, err := pc.AddTrack(pt.local)
			if err != nil {
				log.Printf("sfu: add existing track to %s: %v", tunnelIP, err)
				continue
			}
			go forwardRTCP(rtpSender, pt.publisherPC, pt.remoteSSRC)
		}
		other.mu.Unlock()
	}

	// Request keyframes from all publishers after a short delay
	// so the new subscriber's ICE has time to connect first.
	publishers := make([]*publishedTrack, 0)
	for _, other := range s.participants {
		other.mu.Lock()
		for _, pt := range other.tracks {
			publishers = append(publishers, pt)
		}
		other.mu.Unlock()
	}
	if len(publishers) > 0 {
		go func() {
			time.Sleep(time.Second)
			for _, pt := range publishers {
				_ = pt.publisherPC.WriteRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{MediaSSRC: uint32(pt.remoteSSRC)},
				})
			}
		}()
	}

	pc.OnTrack(func(remote *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		s.handleTrack(p, remote)
	})

	// Chat relay via DataChannel. Each participant creates a DC on their
	// side; when a message arrives, broadcast to all other participants.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		dc.OnOpen(func() {
			p.mu.Lock()
			p.DC = dc
			p.mu.Unlock()
			log.Printf("sfu: chat channel open for %s", tunnelIP)
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			s.broadcastChat(tunnelIP, msg.Data)
		})
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c == nil {
			return
		}
		log.Printf("sfu: ICE candidate for %s: %s %s:%d", tunnelIP, c.Typ, c.Address, c.Port)
		cj := c.ToJSON()
		msg, _ := json.Marshal(signalMsg{
			Kind:      "candidate",
			Candidate: &cj,
			From:      "sfu",
		})
		s.onSignal(tunnelIP, msg)
	})

	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("sfu: %s ICE: %s", tunnelIP, state)
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("sfu: %s connection state: %s", tunnelIP, state)
		if state == webrtc.PeerConnectionStateFailed || state == webrtc.PeerConnectionStateClosed {
			s.RemoveParticipant(tunnelIP)
		}
	})

	s.participants[tunnelIP] = p
	log.Printf("sfu: added participant %s (%s), total=%d", name, tunnelIP, len(s.participants))
	return p, nil
}

func (s *SFU) RemoveParticipant(tunnelIP string) {
	s.mu.Lock()
	p, ok := s.participants[tunnelIP]
	if !ok {
		s.mu.Unlock()
		return
	}
	delete(s.participants, tunnelIP)

	p.mu.Lock()
	removedTracks := make([]*webrtc.TrackLocalStaticRTP, 0, len(p.tracks))
	for _, pt := range p.tracks {
		removedTracks = append(removedTracks, pt.local)
	}
	p.mu.Unlock()

	for _, other := range s.participants {
		for _, t := range removedTracks {
			for _, sender := range other.PC.GetSenders() {
				if sender.Track() == t {
					_ = other.PC.RemoveTrack(sender)
				}
			}
		}
	}

	renegotiateList := make([]*Participant, 0, len(s.participants))
	for _, other := range s.participants {
		renegotiateList = append(renegotiateList, other)
	}
	s.mu.Unlock()

	p.PC.OnConnectionStateChange(func(webrtc.PeerConnectionState) {})
	_ = p.PC.Close()
	log.Printf("sfu: removed participant %s (%s)", p.Name, tunnelIP)

	for _, other := range renegotiateList {
		s.renegotiate(other)
	}
}

// HandleSignal processes a raw JSON WebRTC signal from a participant.
func (s *SFU) HandleSignal(tunnelIP string, body []byte) ([]byte, error) {
	var msg signalMsg
	if err := json.Unmarshal(body, &msg); err != nil {
		return nil, fmt.Errorf("parse signal: %w", err)
	}

	switch msg.Kind {
	case "offer":
		return s.handleOffer(tunnelIP, msg.SDP)
	case "answer":
		return nil, s.handleAnswer(tunnelIP, msg.SDP)
	case "candidate":
		return nil, s.handleCandidateInit(tunnelIP, msg.Candidate)
	default:
		return nil, fmt.Errorf("unknown signal kind: %s", msg.Kind)
	}
}

func (s *SFU) Participants() []ParticipantInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]ParticipantInfo, 0, len(s.participants))
	for _, p := range s.participants {
		out = append(out, ParticipantInfo{
			TunnelIP: p.TunnelIP,
			Name:     p.Name,
		})
	}
	return out
}

func (s *SFU) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.participants {
		_ = p.PC.Close()
	}
	s.participants = make(map[string]*Participant)
	log.Printf("sfu: closed")
}

// --- internal ---

type signalMsg struct {
	Kind      string                   `json:"kind"`
	SDP       string                   `json:"sdp,omitempty"`
	Candidate *webrtc.ICECandidateInit `json:"candidate,omitempty"`
	From      string                   `json:"from,omitempty"`
}

func (s *SFU) handleOffer(tunnelIP, sdp string) ([]byte, error) {
	s.mu.Lock()
	p, ok := s.participants[tunnelIP]
	s.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("unknown participant %s", tunnelIP)
	}

	offer := webrtc.SessionDescription{Type: webrtc.SDPTypeOffer, SDP: sdp}
	if err := p.PC.SetRemoteDescription(offer); err != nil {
		return nil, fmt.Errorf("set remote description: %w", err)
	}

	answer, err := p.PC.CreateAnswer(nil)
	if err != nil {
		return nil, fmt.Errorf("create answer: %w", err)
	}
	if err := p.PC.SetLocalDescription(answer); err != nil {
		return nil, fmt.Errorf("set local description: %w", err)
	}

	resp, _ := json.Marshal(signalMsg{
		Kind: "answer",
		SDP:  answer.SDP,
		From: "sfu",
	})
	return resp, nil
}

func (s *SFU) handleAnswer(tunnelIP, sdp string) error {
	s.mu.Lock()
	p, ok := s.participants[tunnelIP]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown participant %s", tunnelIP)
	}

	log.Printf("sfu: received answer from %s (signalingState=%s)", tunnelIP, p.PC.SignalingState())
	answer := webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdp}
	err := p.PC.SetRemoteDescription(answer)
	if err != nil {
		log.Printf("sfu: setRemoteDescription(answer) for %s failed: %v", tunnelIP, err)
	} else {
		log.Printf("sfu: answer applied for %s, senders=%d", tunnelIP, len(p.PC.GetSenders()))
	}
	return err
}

func (s *SFU) handleCandidateInit(tunnelIP string, candidate *webrtc.ICECandidateInit) error {
	if candidate == nil {
		return nil
	}
	s.mu.Lock()
	p, ok := s.participants[tunnelIP]
	s.mu.Unlock()
	if !ok {
		return fmt.Errorf("unknown participant %s", tunnelIP)
	}
	return p.PC.AddICECandidate(*candidate)
}

func (s *SFU) handleTrack(sender *Participant, remote *webrtc.TrackRemote) {
	log.Printf("sfu: OnTrack from %s: %s (codec=%s, stream=%s)",
		sender.TunnelIP, remote.ID(), remote.Codec().MimeType, remote.StreamID())

	local, err := webrtc.NewTrackLocalStaticRTP(
		remote.Codec().RTPCodecCapability,
		remote.ID(),
		fmt.Sprintf("%s-%s", sender.TunnelIP, remote.StreamID()),
	)
	if err != nil {
		log.Printf("sfu: new local track: %v", err)
		return
	}

	pt := &publishedTrack{
		local:       local,
		publisherPC: sender.PC,
		remoteSSRC:  remote.SSRC(),
	}

	sender.mu.Lock()
	sender.tracks[remote.ID()] = pt
	sender.mu.Unlock()

	s.mu.Lock()
	renegotiateList := make([]*Participant, 0, len(s.participants))
	for _, p := range s.participants {
		if p.TunnelIP == sender.TunnelIP {
			continue
		}
		log.Printf("sfu: forwarding track %s from %s → %s", remote.Codec().MimeType, sender.TunnelIP, p.TunnelIP)
		rtpSender, err := p.PC.AddTrack(local)
		if err != nil {
			log.Printf("sfu: add track to %s: %v", p.TunnelIP, err)
			continue
		}
		renegotiateList = append(renegotiateList, p)
		go forwardRTCP(rtpSender, sender.PC, remote.SSRC())
	}
	s.mu.Unlock()

	for _, p := range renegotiateList {
		s.renegotiate(p)
	}

	// Request keyframes repeatedly to cover the renegotiation window.
	ssrc := remote.SSRC()
	go func() {
		for i := 0; i < 5; i++ {
			time.Sleep(2 * time.Second)
			_ = sender.PC.WriteRTCP([]rtcp.Packet{
				&rtcp.PictureLossIndication{MediaSSRC: uint32(ssrc)},
			})
		}
	}()

	log.Printf("sfu: forwarding %s from %s (ssrc=%d)", remote.Codec().MimeType, sender.TunnelIP, ssrc)
	buf := make([]byte, 1500)
	n, _, err := remote.Read(buf)
	if err != nil {
		log.Printf("sfu: forward loop for %s/%s exited on first read: %v", sender.TunnelIP, remote.ID(), err)
		sender.mu.Lock()
		delete(sender.tracks, remote.ID())
		sender.mu.Unlock()
		return
	}
	log.Printf("sfu: first RTP packet from %s/%s (%d bytes)", sender.TunnelIP, remote.ID(), n)
	_, _ = local.Write(buf[:n])

	for {
		n, _, err = remote.Read(buf)
		if err != nil {
			log.Printf("sfu: forward loop ended for %s/%s: %v", sender.TunnelIP, remote.ID(), err)
			break
		}
		if _, err := local.Write(buf[:n]); err != nil {
			log.Printf("sfu: forward write error for %s/%s: %v", sender.TunnelIP, remote.ID(), err)
			break
		}
	}

	sender.mu.Lock()
	delete(sender.tracks, remote.ID())
	sender.mu.Unlock()

	s.mu.Lock()
	var affectedSubs []*Participant
	for _, sub := range s.participants {
		if sub.TunnelIP == sender.TunnelIP {
			continue
		}
		for _, snd := range sub.PC.GetSenders() {
			if snd.Track() == local {
				_ = sub.PC.RemoveTrack(snd)
				affectedSubs = append(affectedSubs, sub)
				break
			}
		}
	}
	s.mu.Unlock()

	for _, sub := range affectedSubs {
		s.renegotiate(sub)
	}
	log.Printf("sfu: removed ended track %s from %d subscriber(s)", remote.ID(), len(affectedSubs))
}

type renegotiateMsg struct {
	Kind   string          `json:"kind"`
	From   string          `json:"from"`
	Tracks []trackInfoMsg  `json:"tracks,omitempty"`
}

type trackInfoMsg struct {
	Kind string `json:"kind"`
}

func (s *SFU) renegotiate(p *Participant) {
	s.mu.Lock()
	if t, ok := s.renegTimers[p.TunnelIP]; ok {
		t.Stop()
	}
	s.renegTimers[p.TunnelIP] = time.AfterFunc(200*time.Millisecond, func() {
		s.mu.Lock()
		delete(s.renegTimers, p.TunnelIP)

		var tracks []trackInfoMsg
		for _, snd := range p.PC.GetSenders() {
			t := snd.Track()
			if t == nil {
				continue
			}
			tracks = append(tracks, trackInfoMsg{Kind: t.Kind().String()})
		}
		s.mu.Unlock()

		log.Printf("sfu: requesting renegotiation from %s (%d sender tracks)", p.TunnelIP, len(tracks))
		msg, _ := json.Marshal(renegotiateMsg{
			Kind:   "renegotiate",
			From:   "sfu",
			Tracks: tracks,
		})
		s.onSignal(p.TunnelIP, msg)
	})
	s.mu.Unlock()
}

func (s *SFU) broadcastChat(senderIP string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.participants {
		if p.TunnelIP == senderIP {
			continue
		}
		p.mu.Lock()
		dc := p.DC
		p.mu.Unlock()
		if dc != nil && dc.ReadyState() == webrtc.DataChannelStateOpen {
			_ = dc.SendText(string(data))
		}
	}
}

// forwardRTCP reads RTCP from a subscriber's RTPSender and forwards
// PLI/FIR requests to the publisher's PC so the publisher's browser
// generates keyframes when the subscriber needs them.
func forwardRTCP(rtpSender *webrtc.RTPSender, publisherPC *webrtc.PeerConnection, publisherSSRC webrtc.SSRC) {
	for {
		pkts, _, err := rtpSender.ReadRTCP()
		if err != nil {
			return
		}
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
				_ = publisherPC.WriteRTCP([]rtcp.Packet{
					&rtcp.PictureLossIndication{MediaSSRC: uint32(publisherSSRC)},
				})
			}
		}
	}
}

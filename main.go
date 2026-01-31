package main

import (
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sync"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Peer struct {
	id                 int64
	pc                 *webrtc.PeerConnection
	ws                 *websocket.Conn
	wsMu               sync.Mutex
	videoSenders       map[int64]*webrtc.RTPSender
	audioSenders       map[int64]*webrtc.RTPSender
	tracks             map[int64]*webrtc.TrackLocalStaticRTP // sender peer ID -> local track for forwarding
	closed             bool
	needsRenegotiation bool
	negotiating        bool
}

var (
	peersMu sync.Mutex
	peers   = map[int64]*Peer{}
)

func main() {
	http.Handle("/", http.FileServer(http.Dir(".")))
	http.HandleFunc("/ws", wsHandler)

	log.Println("SFU listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.l.google.com:19302"}},
			{
				URLs:       []string{"turn:openrelay.metered.ca:80"},
				Username:   "openrelayproject",
				Credential: "openrelayproject",
			},
		},
	})
	if err != nil {
		return
	}

	peer := &Peer{
		id:           rand.Int63(),
		pc:           pc,
		ws:           ws,
		videoSenders: make(map[int64]*webrtc.RTPSender),
		audioSenders: make(map[int64]*webrtc.RTPSender),
		tracks:       make(map[int64]*webrtc.TrackLocalStaticRTP),
	}

	peersMu.Lock()
	for _, other := range peers {
		// On NEW peer → create senders for existing peers (only if other peer has tracks)
		peer.videoSenders[other.id] = createSender(peer.pc, webrtc.RTPCodecTypeVideo)
		peer.audioSenders[other.id] = createSender(peer.pc, webrtc.RTPCodecTypeAudio)

		// On EXISTING peer → create senders for new peer
		other.videoSenders[peer.id] = createSender(other.pc, webrtc.RTPCodecTypeVideo)
		other.audioSenders[peer.id] = createSender(other.pc, webrtc.RTPCodecTypeAudio)

		// Trigger renegotiation on existing peer to add new senders
		go renegotiate(other)
	}
	peers[peer.id] = peer
	peersMu.Unlock()

	// Add transceivers for RECEIVING this peer's own media ONLY
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{
		Direction: webrtc.RTPTransceiverDirectionRecvonly,
	})

	log.Println("Peer connected, existing peers:", len(peers)-1)

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			peer.wsMu.Lock()
			ws.WriteJSON(map[string]any{
				"type":      "ice",
				"candidate": c.ToJSON(),
			})
			peer.wsMu.Unlock()
		}
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		log.Println("Track received:", track.Kind(), "from peer", peer.id)

		// Create ONE local track for this incoming track
		local, _ := webrtc.NewTrackLocalStaticRTP(
			track.Codec().RTPCodecCapability,
			track.ID(),
			fmt.Sprintf("%d", peer.id), // Use sender's peer ID as stream ID
		)

		// Forward packets from remote track to local track
		go func() {
			buf := make([]byte, 1500)
			for {
				n, _, err := track.Read(buf)
				if err != nil {
					return
				}
				_, _ = local.Write(buf[:n])
			}
		}()

		// Replace track on all other peers
		peersMu.Lock()
		needsRenegotiation := false
		for _, p := range peers {
			if p.id == peer.id || p.closed {
				continue
			}

			if track.Kind() == webrtc.RTPCodecTypeVideo {
				_ = p.videoSenders[peer.id].ReplaceTrack(local)
			} else {
				_ = p.audioSenders[peer.id].ReplaceTrack(local)
			}
			needsRenegotiation = true
		}
		peersMu.Unlock()

		// Trigger renegotiation once after both tracks are ready
		if needsRenegotiation {
			peersMu.Lock()
			for _, p := range peers {
				if p.id != peer.id && !p.closed {
					go renegotiate(p)
				}
			}
			peersMu.Unlock()
		}
	})

	offer, _ := pc.CreateOffer(nil)
	_ = pc.SetLocalDescription(offer)

	peer.wsMu.Lock()
	ws.WriteJSON(map[string]any{
		"type": "offer",
		"sdp":  offer,
	})
	peer.wsMu.Unlock()

	go readLoop(peer)
}

func createSender(pc *webrtc.PeerConnection, kind webrtc.RTPCodecType) *webrtc.RTPSender {
	trans, _ := pc.AddTransceiverFromKind(
		kind,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionSendonly},
	)
	return trans.Sender()
}

func renegotiate(p *Peer) {
	if p.closed {
		return
	}

	// If already negotiating, mark that we need to renegotiate again after this completes
	if p.negotiating {
		p.needsRenegotiation = true
		return
	}

	p.negotiating = true
	p.needsRenegotiation = false

	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		log.Println("Renegotiation offer error:", err)
		p.negotiating = false
		return
	}

	if err := p.pc.SetLocalDescription(offer); err != nil {
		log.Println("Renegotiation set local desc error:", err)
		p.negotiating = false
		return
	}

	p.wsMu.Lock()
	err = p.ws.WriteJSON(map[string]any{
		"type": "offer",
		"sdp":  offer,
	})
	p.wsMu.Unlock()

	if err != nil {
		log.Println("Renegotiation send offer error:", err)
		p.negotiating = false
	}
}

func readLoop(p *Peer) {
	defer func() {
		p.pc.Close()

		peersMu.Lock()
		delete(peers, p.id)
		for _, other := range peers {
			other.ws.WriteJSON(map[string]any{
				"type":   "peer-left",
				"peerId": p.id,
			})
		}
		peersMu.Unlock()

		p.ws.Close()
	}()

	for {
		var msg map[string]any
		if err := p.ws.ReadJSON(&msg); err != nil {
			return
		}

		switch msg["type"] {
		case "answer":
			raw := msg["sdp"].(map[string]any)
			_ = p.pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  raw["sdp"].(string),
			})

			// Negotiation complete
			p.negotiating = false

			// If new tracks arrived during negotiation, renegotiate again
			if p.needsRenegotiation {
				go renegotiate(p)
			}
		case "ice":
			raw := msg["candidate"].(map[string]any)
			_ = p.pc.AddICECandidate(webrtc.ICECandidateInit{
				Candidate: raw["candidate"].(string),
			})
		}
	}
}

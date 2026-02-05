package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"sfu/db"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/joho/godotenv"
	"github.com/pion/rtcp"
	"github.com/pion/webrtc/v3"
	"github.com/redis/go-redis/v9"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

type Peer struct {
	id           int64
	pc           *webrtc.PeerConnection
	ws           *websocket.Conn
	mu           sync.RWMutex
	tracks       map[string]*webrtc.TrackLocalStaticRTP
	peerSenders  map[int64][]*webrtc.RTPSender
	remoteTracks map[string]*webrtc.TrackRemote
	wsMu         sync.Mutex
	roomId       string
	negotiating  bool
}

type Room struct {
	id          string
	peers       map[int64]*Peer
	mu          sync.Mutex
	title       string
	meetingType string
	createdAt   time.Time
	timer       *time.Timer
}

type RoomInfo struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	MeetingType string    `json:"meetingType"`
	PeerCount   int       `json:"peerCount"`
	CreatedAt   time.Time `json:"createdAt"`
	ExpiresAt   time.Time `json:"expiresAt"`
}

var (
	roomsMu     sync.Mutex
	rooms       = map[string]*Room{}
	RedisClient *redis.Client
	ctx         = context.Background()
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("No .env file found")
	}

	var err error
	RedisClient, err = db.ConnectRedis()
	if err != nil {
		log.Fatal("Failed to connect to Redis:", err)
	}
	log.Println("Connected to Redis")

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	http.Handle("/", http.FileServer(http.Dir(".")))
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/rooms", getRoomsHandler)

	log.Printf("SFU listening on :%s", port)
	log.Fatal(http.ListenAndServe(":"+port, nil))
}

// --- Redis Helpers ---
func getMeetingDuration() time.Duration {
	env := os.Getenv("MEETING_DURATION_MINUTES")
	if env == "" {
		return 10 * time.Minute
	}
	min, err := strconv.Atoi(env)
	if err != nil {
		return 10 * time.Minute
	}
	return time.Duration(min) * time.Minute
}

func saveRoomToRedis(room *Room) error {
	roomInfo := RoomInfo{
		ID: room.id, Title: room.title, MeetingType: room.meetingType, PeerCount: len(room.peers),
		CreatedAt: room.createdAt, ExpiresAt: room.createdAt.Add(10 * time.Minute),
	}
	data, _ := json.Marshal(roomInfo)
	return RedisClient.Set(ctx, "room:"+room.id, data, 10*time.Minute).Err()
}

func updateRoomPeerCount(roomId string, peerCount int) error {
	key := "room:" + roomId
	data, err := RedisClient.Get(ctx, key).Bytes()
	if err != nil {
		return err
	}
	var roomInfo RoomInfo
	if json.Unmarshal(data, &roomInfo) != nil {
		return err
	}
	roomInfo.PeerCount = peerCount
	newData, _ := json.Marshal(roomInfo)
	return RedisClient.Set(ctx, key, newData, RedisClient.TTL(ctx, key).Val()).Err()
}

func deleteRoomFromRedis(roomId string) error { return RedisClient.Del(ctx, "room:"+roomId).Err() }

func getAllRoomsFromRedis() ([]RoomInfo, error) {
	keys, err := RedisClient.Keys(ctx, "room:*").Result()
	if err != nil {
		return nil, err
	}
	var roomInfos []RoomInfo
	for _, key := range keys {
		data, err := RedisClient.Get(ctx, key).Bytes()
		if err == nil {
			var info RoomInfo
			if json.Unmarshal(data, &info) == nil {
				roomInfos = append(roomInfos, info)
			}
		}
	}
	return roomInfos, nil
}

func getRoomsHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	rooms, _ := getAllRoomsFromRedis()
	json.NewEncoder(w).Encode(rooms)
}

// --- Video Logic ---

func requestKeyframe(sourcePeer *Peer, ssrc uint32) {
	if sourcePeer.pc == nil || sourcePeer.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
		return
	}
	sourcePeer.pc.WriteRTCP([]rtcp.Packet{
		&rtcp.PictureLossIndication{MediaSSRC: ssrc},
	})
}

func forwardPLI(sender *webrtc.RTPSender, sourcePeer *Peer, trackID string) {
	go func() {
		rtcpBuf := make([]byte, 1500)
		for {
			n, _, err := sender.Read(rtcpBuf)
			if err != nil {
				return
			}
			packets, err := rtcp.Unmarshal(rtcpBuf[:n])
			if err != nil {
				continue
			}
			for _, p := range packets {
				switch p.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					sourcePeer.mu.RLock()
					if remoteTrack, ok := sourcePeer.remoteTracks[trackID]; ok {
						requestKeyframe(sourcePeer, uint32(remoteTrack.SSRC()))
					}
					sourcePeer.mu.RUnlock()
				}
			}
		}
	}()
}

// --- WebSocket Handler ---

func wsHandler(w http.ResponseWriter, r *http.Request) {
	roomId := r.URL.Query().Get("room")
	if roomId == "" {
		roomId = "default"
	}
	title := r.URL.Query().Get("title")
	meetingType := r.URL.Query().Get("type")

	ws, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType: webrtc.MimeTypeVP8, ClockRate: 90000, Channels: 0, SDPFmtpLine: "",
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "goog-remb"}, {Type: "ccm", Parameter: "fir"}, {Type: "nack"}, {Type: "nack", Parameter: "pli"},
			},
		},
		PayloadType: 96,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return
	}

	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: 48000, Channels: 2, SDPFmtpLine: "", RTCPFeedback: nil},
		PayloadType:        111,
	}, webrtc.RTPCodecTypeAudio); err != nil {
		return
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))

	pc, err := api.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{
			{URLs: []string{"stun:stun.relay.metered.ca:80"}},
			{
				URLs:       []string{"turn:global.relay.metered.ca:80"},
				Username:   "08c57540fdb60bd386988d52",
				Credential: "Vn+jLmPG9MzlDKpi",
			},
			{
				URLs:       []string{"turn:global.relay.metered.ca:80?transport=tcp"},
				Username:   "08c57540fdb60bd386988d52",
				Credential: "Vn+jLmPG9MzlDKpi",
			},
			{
				URLs:       []string{"turn:global.relay.metered.ca:443"},
				Username:   "08c57540fdb60bd386988d52",
				Credential: "Vn+jLmPG9MzlDKpi",
			},
			{
				URLs:       []string{"turns:global.relay.metered.ca:443?transport=tcp"},
				Username:   "08c57540fdb60bd386988d52",
				Credential: "Vn+jLmPG9MzlDKpi",
			},
		},
		ICETransportPolicy: webrtc.ICETransportPolicyRelay,
	})
	if err != nil {
		return
	}

	peer := &Peer{
		id: rand.Int63(), pc: pc, ws: ws,
		tracks:       make(map[string]*webrtc.TrackLocalStaticRTP),
		peerSenders:  make(map[int64][]*webrtc.RTPSender),
		remoteTracks: make(map[string]*webrtc.TrackRemote),
		roomId:       roomId,
	}

	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})

	roomsMu.Lock()
	room, exists := rooms[roomId]
	if !exists {
		room = &Room{id: roomId, peers: make(map[int64]*Peer), title: title, meetingType: meetingType, createdAt: time.Now()}
		rooms[roomId] = room
		saveRoomToRedis(room)
		room.timer = time.AfterFunc(10*time.Minute, func() { endRoom(room) })
	}
	roomsMu.Unlock()

	room.mu.Lock()
	// Add existing tracks to new peer
	for _, other := range room.peers {
		other.mu.RLock()
		for _, track := range other.tracks {
			sender, err := peer.pc.AddTrack(track)
			if err != nil {
				continue
			}
			peer.mu.Lock()
			peer.peerSenders[other.id] = append(peer.peerSenders[other.id], sender)
			peer.mu.Unlock()
			if track.Kind() == webrtc.RTPCodecTypeVideo {
				forwardPLI(sender, other, track.ID())
				// Request keyframe for immediate video
				go func(src *Peer, trackID string) {
					time.Sleep(100 * time.Millisecond)
					src.mu.RLock()
					if remoteTrack, ok := src.remoteTracks[trackID]; ok {
						requestKeyframe(src, uint32(remoteTrack.SSRC()))
					}
					src.mu.RUnlock()
				}(other, track.ID())
			}
		}
		other.mu.RUnlock()
	}
	room.peers[peer.id] = peer
	updateRoomPeerCount(roomId, len(room.peers))
	room.mu.Unlock()

	// Log ICE connection state changes for debugging
	pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		log.Printf("Peer %d ICE state: %s", peer.id, state.String())
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("Peer %d connection state: %s", peer.id, state.String())
	})

	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			log.Printf("Peer %d sending ICE candidate: %s", peer.id, c.ToJSON().Candidate)
			peer.wsMu.Lock()
			ws.WriteJSON(map[string]any{"type": "ice", "candidate": c.ToJSON()})
			peer.wsMu.Unlock()
		}
	})

	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		peer.mu.Lock()
		peer.remoteTracks[track.ID()] = track
		peer.mu.Unlock()

		streamId := fmt.Sprintf("%d", peer.id)
		local, err := webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, track.ID(), streamId)
		if err != nil {
			return
		}

		peer.mu.Lock()
		peer.tracks[track.ID()] = local
		peer.mu.Unlock()

		// Start reading packets immediately
		go func() {
			buf := make([]byte, 1500)
			for {
				n, _, err := track.Read(buf)
				if err != nil {
					return
				}
				if _, err = local.Write(buf[:n]); err != nil {
					return
				}
			}
		}()

		// Distribute to other peers
		roomsMu.Lock()
		r := rooms[peer.roomId]
		roomsMu.Unlock()

		if r != nil {
			r.mu.Lock()
			peersToUpdate := make([]*Peer, 0)
			for _, other := range r.peers {
				if other.id == peer.id {
					continue
				}
				sender, err := other.pc.AddTrack(local)
				if err != nil {
					log.Printf("Failed to add track to peer %d: %v", other.id, err)
					continue
				}

				other.mu.Lock()
				other.peerSenders[peer.id] = append(other.peerSenders[peer.id], sender)
				other.mu.Unlock()

				if track.Kind() == webrtc.RTPCodecTypeVideo {
					forwardPLI(sender, peer, track.ID())
				}
				peersToUpdate = append(peersToUpdate, other)
			}
			r.mu.Unlock()

			// Renegotiate after releasing locks to avoid deadlock
			for _, other := range peersToUpdate {
				go renegotiate(other)
			}

			// Request keyframe after peers have renegotiated
			if track.Kind() == webrtc.RTPCodecTypeVideo {
				go func(src *Peer, ssrc uint32) {
					time.Sleep(500 * time.Millisecond) // Wait for renegotiation
					requestKeyframe(src, ssrc)
					time.Sleep(1 * time.Second) // Request again for reliability
					requestKeyframe(src, ssrc)
				}(peer, uint32(track.SSRC()))
			}
		}
	})

	renegotiate(peer)
	go readLoop(peer)
}

func endRoom(room *Room) {
	room.mu.Lock()
	peers := make([]*Peer, 0, len(room.peers))
	for _, p := range room.peers {
		peers = append(peers, p)
	}
	room.mu.Unlock()
	for _, peer := range peers {
		peer.wsMu.Lock()
		peer.ws.WriteJSON(map[string]any{"type": "room-ended", "message": "Time limit reached"})
		peer.wsMu.Unlock()
		peer.pc.Close()
		peer.ws.Close()
	}
	roomsMu.Lock()
	delete(rooms, room.id)
	roomsMu.Unlock()
	deleteRoomFromRedis(room.id)
}

func renegotiate(p *Peer) {
	// Small delay to batch multiple track additions
	time.Sleep(50 * time.Millisecond)

	p.wsMu.Lock()
	defer p.wsMu.Unlock()
	if p.negotiating {
		return
	}

	// Check if peer connection is still valid
	if p.pc == nil || p.pc.ConnectionState() == webrtc.PeerConnectionStateClosed {
		return
	}

	p.negotiating = true
	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		log.Printf("Failed to create offer for peer %d: %v", p.id, err)
		p.negotiating = false
		return
	}

	err = p.pc.SetLocalDescription(offer)
	if err != nil {
		log.Printf("Failed to set local description for peer %d: %v", p.id, err)
		p.negotiating = false
		return
	}

	err = p.ws.WriteJSON(map[string]any{"type": "offer", "sdp": offer})
	if err != nil {
		log.Printf("Failed to send offer to peer %d: %v", p.id, err)
		p.negotiating = false
	}
}

func readLoop(p *Peer) {
	defer func() {
		p.pc.Close()
		roomsMu.Lock()
		if room, ok := rooms[p.roomId]; ok {
			room.mu.Lock()
			delete(room.peers, p.id)
			for _, other := range room.peers {
				other.mu.Lock()
				if senders, ok := other.peerSenders[p.id]; ok {
					for _, s := range senders {
						other.pc.RemoveTrack(s)
					}
					delete(other.peerSenders, p.id)
					go renegotiate(other)
				}
				other.mu.Unlock()
				other.wsMu.Lock()
				other.ws.WriteJSON(map[string]any{"type": "peer-left", "peerId": fmt.Sprintf("%d", p.id)})
				other.wsMu.Unlock()
			}
			updateRoomPeerCount(p.roomId, len(room.peers))
			if len(room.peers) == 0 {
				if room.timer != nil {
					room.timer.Stop()
				}
				delete(rooms, p.roomId)
				deleteRoomFromRedis(p.roomId)
			}
			room.mu.Unlock()
		}
		roomsMu.Unlock()
	}()
	for {
		var msg map[string]any
		if err := p.ws.ReadJSON(&msg); err != nil {
			return
		}
		switch msg["type"] {
		case "answer":
			sdpStr, _ := msg["sdp"].(map[string]any)["sdp"].(string)
			// Lower bitrate to reduce delay
			sdpStr = strings.ReplaceAll(sdpStr, "useinbandfec=1", "useinbandfec=1;x-google-max-bitrate=800")
			p.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: sdpStr})
			p.wsMu.Lock()
			p.negotiating = false
			p.wsMu.Unlock()
		case "ice":
			raw := msg["candidate"].(map[string]any)
			c, _ := raw["candidate"].(string)
			mid, _ := raw["sdpMid"].(string)
			idx := uint16(raw["sdpMLineIndex"].(float64))
			p.pc.AddICECandidate(webrtc.ICECandidateInit{Candidate: c, SDPMid: &mid, SDPMLineIndex: &idx})
		}
	}
}

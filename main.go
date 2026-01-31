package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"sfu/db"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/pion/webrtc/v3"
	"github.com/redis/go-redis/v9"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool { return true },
}

// Peer represents a connected client
type Peer struct {
	id   int64
	pc   *webrtc.PeerConnection
	ws   *websocket.Conn
	wsMu sync.Mutex

	// Local tracks that this peer is SENDING to the server
	tracks map[string]*webrtc.TrackLocalStaticRTP // map[trackID]track

	// Senders on this peer's PC that are sending OTHER peers' media TO this peer
	// Map: SourcePeerID -> Slice of Senders
	peerSenders map[int64][]*webrtc.RTPSender

	roomId             string
	negotiating        bool
	needsRenegotiation bool
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
	var err error
	RedisClient, err = db.ConnectRedis()
	if err != nil {
		log.Fatal("Failed to connect to Redis:", err)
	}
	log.Println("Connected to Redis")

	http.Handle("/", http.FileServer(http.Dir(".")))
	http.HandleFunc("/ws", wsHandler)
	http.HandleFunc("/rooms", getRoomsHandler)

	log.Println("SFU listening on :8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// --- Redis & Room Helpers (Same as before) ---

func saveRoomToRedis(room *Room) error {
	roomInfo := RoomInfo{
		ID:          room.id,
		Title:       room.title,
		MeetingType: room.meetingType,
		PeerCount:   len(room.peers),
		CreatedAt:   room.createdAt,
		ExpiresAt:   room.createdAt.Add(10 * time.Minute),
	}
	data, err := json.Marshal(roomInfo)
	if err != nil {
		return err
	}
	key := "room:" + room.id
	return RedisClient.Set(ctx, key, data, 10*time.Minute).Err()
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
	ttl := RedisClient.TTL(ctx, key).Val()
	return RedisClient.Set(ctx, key, newData, ttl).Err()
}

func deleteRoomFromRedis(roomId string) error {
	return RedisClient.Del(ctx, "room:"+roomId).Err()
}

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
	rooms, err := getAllRoomsFromRedis()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(rooms)
}

func endRoom(room *Room) {
	log.Println("Room", room.id, "has expired")
	room.mu.Lock()
	// Copy peers to avoid map modification issues
	peers := make([]*Peer, 0, len(room.peers))
	for _, p := range room.peers {
		peers = append(peers, p)
	}
	room.mu.Unlock()

	for _, peer := range peers {
		peer.wsMu.Lock()
		peer.ws.WriteJSON(map[string]any{"type": "room-ended", "message": "Meeting time limit reached"})
		peer.wsMu.Unlock()
		peer.pc.Close()
		peer.ws.Close()
	}

	roomsMu.Lock()
	delete(rooms, room.id)
	roomsMu.Unlock()
	deleteRoomFromRedis(room.id)
}

// --- WebSocket & SFU Logic ---

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
		id:          rand.Int63(),
		pc:          pc,
		ws:          ws,
		tracks:      make(map[string]*webrtc.TrackLocalStaticRTP),
		peerSenders: make(map[int64][]*webrtc.RTPSender),
		roomId:      roomId,
	}

	// Add transceivers for receiving media from THIS peer
	// We want to receive 1 audio and 1 video from the client
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeVideo, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})
	pc.AddTransceiverFromKind(webrtc.RTPCodecTypeAudio, webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly})

	// Join Room logic
	roomsMu.Lock()
	room, exists := rooms[roomId]
	if !exists {
		room = &Room{
			id: roomId, peers: make(map[int64]*Peer),
			title: title, meetingType: meetingType, createdAt: time.Now(),
		}
		rooms[roomId] = room
		saveRoomToRedis(room)
		room.timer = time.AfterFunc(10*time.Minute, func() { endRoom(room) })
		log.Println("Created room:", roomId)
	}
	roomsMu.Unlock()

	room.mu.Lock()

	// 1. Add Existing Peers' tracks to New Peer (Fix for Latency)
	for _, other := range room.peers {
		for _, track := range other.tracks {
			// AddTrack auto-creates a SendOnly transceiver
			sender, err := peer.pc.AddTrack(track)
			if err != nil {
				log.Printf("Failed to add track from %d to %d: %v", other.id, peer.id, err)
				continue
			}
			peer.peerSenders[other.id] = append(peer.peerSenders[other.id], sender)
		}
	}

	room.peers[peer.id] = peer
	updateRoomPeerCount(roomId, len(room.peers))
	room.mu.Unlock()

	log.Printf("Peer %d joined room %s", peer.id, roomId)

	// ICE Handling
	pc.OnICECandidate(func(c *webrtc.ICECandidate) {
		if c != nil {
			peer.wsMu.Lock()
			ws.WriteJSON(map[string]any{"type": "ice", "candidate": c.ToJSON()})
			peer.wsMu.Unlock()
		}
	})

	// OnTrack: Handle INCOMING streams from this peer
	pc.OnTrack(func(track *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		log.Printf("Peer %d sent track: %s (%s)", peer.id, track.ID(), track.Kind())

		// Create local track to forward to others
		// IMPORTANT: Use peer.id as StreamID so frontend groups A/V correctly
		streamId := fmt.Sprintf("%d", peer.id)
		local, err := webrtc.NewTrackLocalStaticRTP(track.Codec().RTPCodecCapability, track.ID(), streamId)
		if err != nil {
			log.Println("NewTrackLocalStaticRTP error:", err)
			return
		}

		peer.tracks[track.ID()] = local

		// Start forwarding loop
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

		// Broadcast this new track to all existing peers
		roomsMu.Lock()
		r := rooms[peer.roomId]
		roomsMu.Unlock()
		if r == nil {
			return
		}

		r.mu.Lock()
		for _, other := range r.peers {
			if other.id == peer.id {
				continue
			}

			// Add the new track to the other peer
			sender, err := other.pc.AddTrack(local)
			if err != nil {
				log.Println("AddTrack error:", err)
				continue
			}
			other.peerSenders[peer.id] = append(other.peerSenders[peer.id], sender)

			// Renegotiate so they see the new track
			go renegotiate(other)
		}
		r.mu.Unlock()
	})

	// Send Initial Offer (includes any existing tracks we added in step 1)
	renegotiate(peer)

	go readLoop(peer)
}

func renegotiate(p *Peer) {
	p.wsMu.Lock()
	defer p.wsMu.Unlock()

	if p.negotiating {
		p.needsRenegotiation = true
		return
	}
	p.negotiating = true

	offer, err := p.pc.CreateOffer(nil)
	if err != nil {
		log.Println("CreateOffer error:", err)
		p.negotiating = false
		return
	}

	if err := p.pc.SetLocalDescription(offer); err != nil {
		log.Println("SetLocalDescription error:", err)
		p.negotiating = false
		return
	}

	if err := p.ws.WriteJSON(map[string]any{"type": "offer", "sdp": offer}); err != nil {
		log.Println("WriteJSON error:", err)
		p.negotiating = false
	}
}

func readLoop(p *Peer) {
	defer func() {
		// Cleanup
		p.pc.Close()

		roomsMu.Lock()
		room := rooms[p.roomId]
		roomsMu.Unlock()

		if room != nil {
			room.mu.Lock()
			delete(room.peers, p.id)

			// Remove this peer's tracks from everyone else
			for _, other := range room.peers {
				if senders, ok := other.peerSenders[p.id]; ok {
					for _, sender := range senders {
						other.pc.RemoveTrack(sender)
					}
					delete(other.peerSenders, p.id)
					go renegotiate(other) // Trigger update for removal
				}

				// Notify frontend
				other.wsMu.Lock()
				other.ws.WriteJSON(map[string]any{"type": "peer-left", "peerId": fmt.Sprintf("%d", p.id)})
				other.wsMu.Unlock()
			}

			count := len(room.peers)
			updateRoomPeerCount(p.roomId, count)

			if count == 0 {
				if room.timer != nil {
					room.timer.Stop()
				}
				deleteRoomFromRedis(p.roomId)
				roomsMu.Lock()
				delete(rooms, p.roomId)
				roomsMu.Unlock()
				room.mu.Unlock() // unlock before log/return
				log.Println("Room destroyed:", p.roomId)
				return
			}
			room.mu.Unlock()
		}
		p.ws.Close()
	}()

	for {
		var msg map[string]any
		if err := p.ws.ReadJSON(&msg); err != nil {
			return
		}

		switch msg["type"] {
		case "answer":
			sdpStr, _ := msg["sdp"].(map[string]any)["sdp"].(string)
			if err := p.pc.SetRemoteDescription(webrtc.SessionDescription{
				Type: webrtc.SDPTypeAnswer,
				SDP:  sdpStr,
			}); err != nil {
				log.Println("SetRemoteDesc error:", err)
				continue
			}

			// Lock strictly for flag update to avoid race
			p.wsMu.Lock()
			p.negotiating = false
			if p.needsRenegotiation {
				p.needsRenegotiation = false
				p.wsMu.Unlock()
				go renegotiate(p)
			} else {
				p.wsMu.Unlock()
			}

		case "ice":
			raw := msg["candidate"].(map[string]any)
			candidateStr, _ := raw["candidate"].(string)
			sdpMid, _ := raw["sdpMid"].(string)
			sdpMLineIndex := uint16(raw["sdpMLineIndex"].(float64))

			p.pc.AddICECandidate(webrtc.ICECandidateInit{
				Candidate:     candidateStr,
				SDPMid:        &sdpMid,
				SDPMLineIndex: &sdpMLineIndex,
			})
		}
	}
}

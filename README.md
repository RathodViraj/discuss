# Go WebRTC SFU
**Demo: https://youtu.be/S59ji1Q2RS0**

This project is a lightweight WebRTC Selective Forwarding Unit (SFU) written in Go using the Pion WebRTC library, featuring a browser-based frontend interface.

## How It Works

### Selective Forwarding Unit (SFU) Architecture
Instead of utilizing a mesh topology (where every peer establishes direct connections with every other peer) or a Multipoint Control Unit (MCU, where the server decodes, mixes, and re-encodes all streams), this application implements an SFU architecture:
- Each participant establishes a single WebRTC Peer Connection with the Go server and publishes their media streams (microphone, camera, or screen share).
- The Go server acts as a media router. It receives the incoming RTP packets from the sender, replicates them at the network layer without decoding the media payload, and routes them to all other participants in the room.
- This design scales efficiently as upload bandwidth for each client remains constant regardless of the number of participants.

### Signaling and Connection Establishment
The signaling process coordinates the WebRTC peer connection setup over WebSockets:
- When a user enters a room, a WebSocket connection is opened with the `/ws` handler.
- The server and client exchange Session Description Protocol (SDP) offers and answers, alongside Interactive Connectivity Establishment (ICE) candidates to locate optimal network paths (using STUN/TURN servers).
- The server assigns a unique random identifier to each peer session.

### Track Distribution and Dynamic Renegotiation
- **Joining**: When a new peer joins, the server iterates through all existing participants in the room and registers their local static RTP tracks on the new peer's connection.
- **Publishing Tracks**: When the server receives an incoming media track from a client, the `OnTrack` handler wraps the remote track into a `TrackLocalStaticRTP`. The server then distributes this track to all other peers in the room by adding it to their WebRTC connection.
- **Renegotiation**: Any modification of tracks on an active connection triggers a renegotiation routine (`renegotiate`) where new SDP offers and answers are exchanged over the WebSocket signaling channel.

### RTCP Feedback and Keyframe Handling
- To handle packet loss or new participant entry without waiting for natural keyframe intervals, the SFU forwards RTCP feedback.
- The server reads RTCP packets from peer senders. If it intercepts a Picture Loss Indication (PLI) or a Full Intra Request (FIR) from a receiver, it forwards a keyframe request back to the source publisher.
- Keyframe requests are also triggered automatically immediately after track negotiation to guarantee fast video rendering for new viewers.

### Room Management and Session Lifecycle
- Room metadata (such as room ID, title, meeting type, and active participant counts) is tracked in memory and persisted in a Redis cache for horizontal visibility.
- A room-specific timer automatically schedules room termination after a set duration (configured via `MEETING_DURATION_MINUTES`), closing all active WebSocket and WebRTC peer connections.

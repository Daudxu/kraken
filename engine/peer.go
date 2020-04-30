package engine

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/MixinNetwork/mixin/logger"
	"github.com/gofrs/uuid"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v2"
)

const (
	peerTrackClosedId          = "CLOSED"
	peerTrackConnectionTimeout = 10 * time.Second
	peerTrackReadTimeout       = 3 * time.Second
	rtpBufferSize              = 65536
	rtpClockRate               = 48000
	rtpPacketSequenceMax       = ^uint16(0)
	rtpPacketExpiration        = rtpClockRate / 2
)

type Sender struct {
	id  string
	rtp *webrtc.RTPSender
}

type NackRequest struct {
	uid  string
	cid  string
	pair *rtcp.NackPair
}

type Peer struct {
	sync.RWMutex
	rid         string
	uid         string
	cid         string
	pc          *webrtc.PeerConnection
	track       *webrtc.Track
	publishers  map[string]*Sender
	subscribers map[string]*Sender
	buffer      [rtpBufferSize]*rtp.Packet
	lost        chan *rtp.Header
	queue       chan *rtp.Packet
	nack        chan *NackRequest
	timestamp   uint32
	sequence    uint16
	connected   chan bool
}

func (engine *Engine) BuildPeer(rid, uid string, pc *webrtc.PeerConnection) *Peer {
	cid, err := uuid.NewV4()
	if err != nil {
		panic(err)
	}
	peer := &Peer{rid: rid, uid: uid, cid: cid.String(), pc: pc}
	peer.connected = make(chan bool, 1)
	peer.lost = make(chan *rtp.Header, 17)
	peer.queue = make(chan *rtp.Packet, 48000)
	peer.nack = make(chan *NackRequest, 48000)
	peer.publishers = make(map[string]*Sender)
	peer.subscribers = make(map[string]*Sender)
	peer.handle()
	return peer
}

func (p *Peer) id() string {
	return fmt.Sprintf("%s:%s:%s", p.rid, p.uid, p.cid)
}

func (p *Peer) Close() error {
	logger.Printf("PeerClose(%s) now\n", p.id())
	p.Lock()
	p.track = nil
	p.cid = peerTrackClosedId
	err := p.pc.Close()
	p.Unlock()
	logger.Printf("PeerClose(%s) with %v\n", p.id(), err)
	return err
}

func (peer *Peer) handle() {
	go func() {
		select {
		case <-peer.connected:
		case <-time.After(peerTrackConnectionTimeout):
			logger.Printf("HandlePeer(%s) OnTrackTimeout()\n", peer.id())
			peer.Close()
		}
	}()

	peer.pc.OnSignalingStateChange(func(state webrtc.SignalingState) {
		logger.Printf("HandlePeer(%s) OnSignalingStateChange(%s)\n", peer.id(), state)
	})
	peer.pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		logger.Printf("HandlePeer(%s) OnConnectionStateChange(%s)\n", peer.id(), state)
	})
	peer.pc.OnICEConnectionStateChange(func(state webrtc.ICEConnectionState) {
		logger.Printf("HandlePeer(%s) OnICEConnectionStateChange(%s)\n", peer.id(), state)
	})
	peer.pc.OnTrack(func(rt *webrtc.Track, receiver *webrtc.RTPReceiver) {
		logger.Printf("HandlePeer(%s) OnTrack(%d, %d)\n", peer.id(), rt.PayloadType(), rt.SSRC())
		if peer.track != nil || webrtc.DefaultPayloadTypeOpus != rt.PayloadType() {
			return
		}
		peer.connected <- true

		peer.Lock()
		lt, err := peer.pc.NewTrack(rt.PayloadType(), rt.SSRC(), peer.cid, peer.uid)
		if err != nil {
			panic(err)
		}
		peer.track = lt
		peer.Unlock()

		err = peer.copyTrack(rt, lt)
		logger.Printf("HandlePeer(%s) OnTrack(%d, %d) end with %s\n", peer.id(), rt.PayloadType(), rt.SSRC(), err.Error())
		peer.Close()
	})
}

func (peer *Peer) copyTrack(src, dst *webrtc.Track) error {
	go func() error {
		for {
			pkt, err := src.ReadRTP()
			if err == io.EOF {
				return nil
			}
			if err != nil {
				return err
			}
			peer.queue <- pkt
		}
	}()

	go func() error {
		ticker := time.NewTicker(rtpPacketExpiration / 4)
		defer ticker.Stop()

		lost := make([]*rtp.Header, 0)
		for track := peer.track; track != nil; {
			select {
			case p := <-peer.lost:
				lost = append(lost, p)
			case <-ticker.C:
			}
			if len(lost) == 0 {
				continue
			}
			fsn := lost[0]
			if len(lost) < 16 && fsn.Timestamp+rtpPacketExpiration/4 > peer.timestamp {
				continue
			}
			blp := uint16(0)
			pair := rtcp.NackPair{PacketID: fsn.SequenceNumber}
			for _, p := range lost {
				if p.SequenceNumber <= pair.PacketID {
					continue
				}
				blp = blp | (1 << (p.SequenceNumber - pair.PacketID - 1))
			}
			pair.LostPackets = rtcp.PacketBitmap(blp)
			pkt := &rtcp.TransportLayerNack{
				SenderSSRC: fsn.SSRC,
				MediaSSRC:  fsn.SSRC,
				Nacks:      []rtcp.NackPair{pair},
			}
			err := peer.pc.WriteRTCP([]rtcp.Packet{pkt})
			if err != nil {
				return err
			}
			lost = make([]*rtp.Header, 0)
		}
		return nil
	}()

	for {
		timer := time.NewTimer(peerTrackReadTimeout)
		select {
		case r := <-peer.nack:
			peer.handleNack(r)
		case pkt := <-peer.queue:
			peer.handlePacket(dst, pkt)
		case <-timer.C:
			return fmt.Errorf("peer track read timeout")
		}
		timer.Stop()
	}
}

func (peer *Peer) LoopRTCP(uid string, sender *Sender) error {
	for {
		pkts, err := sender.rtp.ReadRTCP()
		if err != nil {
			logger.Printf("LoopRTCP(%s,%s,%s) with %v\n", peer.id(), uid, sender.id, err)
			return err
		}
		for _, pkt := range pkts {
			switch pkt.(type) {
			case *rtcp.TransportLayerNack:
				nack := pkt.(*rtcp.TransportLayerNack)
				for _, pair := range nack.Nacks {
					logger.Verbosef("LoopRTCP(%s,%s,%s) TransportLayerNack %v\n", peer.id(), uid, sender.id, pair.PacketList())
					peer.nack <- &NackRequest{uid: uid, cid: sender.id, pair: &pair}
				}
			default:
			}
		}
	}
}

func (peer *Peer) handlePacket(dst *webrtc.Track, pkt *rtp.Packet) error {
	old := peer.buffer[pkt.SequenceNumber]
	if old != nil && old.Timestamp >= pkt.Timestamp {
		return nil
	}
	if peer.timestamp > pkt.Timestamp+rtpPacketExpiration {
		return nil
	}
	if peer.timestamp == pkt.Timestamp {
		return nil
	}
	if pkt.Timestamp > peer.timestamp {
		peer.handleLost(pkt)
		peer.timestamp = pkt.Timestamp
		peer.sequence = pkt.SequenceNumber
	}
	peer.buffer[pkt.SequenceNumber] = pkt
	return dst.WriteRTP(pkt)
}

func (peer *Peer) handleLost(pkt *rtp.Packet) error {
	gap := pkt.SequenceNumber - peer.sequence
	if pkt.SequenceNumber < peer.sequence {
		gap = rtpPacketSequenceMax - peer.sequence + pkt.SequenceNumber + 1
	}
	if peer.timestamp+rtpPacketExpiration/2 < pkt.Timestamp {
		return nil
	}
	next := (uint32(peer.sequence) + 1) % 65536
	if gap > 17 {
		next = (uint32(peer.sequence) + uint32(gap-17)) % 65536
		gap = 17
	}
	if next+uint32(gap) > 65536 {
		gap = uint16((next + uint32(gap)) % 65536)
		next = 0
	}
	for i := uint16(1); i < gap; i++ {
		peer.lost <- &rtp.Header{
			SequenceNumber: uint16(next),
			Timestamp:      peer.timestamp,
			SSRC:           pkt.SSRC,
		}
		next = next + 1
	}
	return nil
}

func (peer *Peer) handleNack(r *NackRequest) error {
	peer.RLock()
	sender := peer.subscribers[r.uid]
	peer.RUnlock()

	if sender == nil || sender.id != r.cid {
		return nil
	}

	for _, seq := range r.pair.PacketList() {
		pkt := peer.buffer[seq]
		if pkt == nil {
			continue
		}
		if peer.timestamp > pkt.Timestamp+rtpPacketExpiration {
			continue
		}
		i, err := sender.rtp.SendRTP(&pkt.Header, pkt.Payload)
		logger.Verbosef("HandleNack(%s,%s,%s,%d) with %d %v\n", peer.id(), r.uid, r.cid, seq, i, err)
	}
	return nil
}

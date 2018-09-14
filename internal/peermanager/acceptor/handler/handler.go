package handler

import (
	"net"

	"github.com/cenkalti/rain/internal/bitfield"
	"github.com/cenkalti/rain/internal/btconn"
	"github.com/cenkalti/rain/internal/logger"
	"github.com/cenkalti/rain/internal/peer"
	"github.com/cenkalti/rain/internal/peermanager/peerids"
)

type Handler struct {
	conn     net.Conn
	peerIDs  *peerids.PeerIDs
	bitfield *bitfield.Bitfield
	peerID   [20]byte
	sKeyHash [20]byte
	infoHash [20]byte
	messages *peer.Messages
	log      logger.Logger
}

func New(conn net.Conn, peerIDs *peerids.PeerIDs, bf *bitfield.Bitfield, peerID, sKeyHash, infoHash [20]byte, messages *peer.Messages, l logger.Logger) *Handler {
	return &Handler{
		conn:     conn,
		peerIDs:  peerIDs,
		bitfield: bf,
		peerID:   peerID,
		sKeyHash: sKeyHash,
		infoHash: infoHash,
		messages: messages,
		log:      l,
	}
}

func (h *Handler) Run(stopC chan struct{}) {
	log := logger.New("peer <- " + h.conn.RemoteAddr().String())

	// TODO get this from config
	encryptionForceIncoming := false

	ourExtensions := [8]byte{}
	ourbf := bitfield.NewBytes(ourExtensions[:], 64)
	ourbf.Set(61) // Fast Extension

	// TODO close conn during handshake when stopC is closed
	encConn, cipher, peerExtensions, peerID, _, err := btconn.Accept(
		h.conn, h.getSKey, encryptionForceIncoming, h.checkInfoHash, ourExtensions, h.peerID)
	if err != nil {
		log.Error(err)
		_ = h.conn.Close()
		return
	}
	log.Infof("Connection accepted. (cipher=%s extensions=%x client=%q)", cipher, peerExtensions, peerID[:8])

	ok := h.peerIDs.Add(peerID)
	if !ok {
		_ = h.conn.Close()
		return
	}
	defer h.peerIDs.Remove(peerID)

	peerbf := bitfield.NewBytes(peerExtensions[:], 64)
	extensions := ourbf.And(peerbf)

	p := peer.New(encConn, peerID, extensions, h.bitfield, log, h.messages)
	p.Run(stopC)
}

func (h *Handler) getSKey(sKeyHash [20]byte) []byte {
	if sKeyHash == h.sKeyHash {
		return h.infoHash[:]
	}
	return nil
}

func (h *Handler) checkInfoHash(infoHash [20]byte) bool {
	return infoHash == h.infoHash
}

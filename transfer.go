package rain

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sync"

	"github.com/cenkalti/mse"

	"github.com/cenkalti/rain/internal/bitfield"
	"github.com/cenkalti/rain/internal/connection"
	"github.com/cenkalti/rain/internal/logger"
	"github.com/cenkalti/rain/internal/protocol"
	"github.com/cenkalti/rain/internal/torrent"
	"github.com/cenkalti/rain/internal/tracker"
)

// transfer represents an active transfer in the program.
type transfer struct {
	rain     *Rain
	tracker  tracker.Tracker
	torrent  *torrent.Torrent
	pieces   []*piece
	bitField bitfield.BitField // pieces that we have
	Finished chan struct{}
	haveC    chan peerHave
	peers    map[*peer]struct{}
	peersM   sync.RWMutex
	log      logger.Logger
}

func (r *Rain) newTransfer(tor *torrent.Torrent, where string) (*transfer, error) {
	tracker, err := tracker.New(tor.Announce, r)
	if err != nil {
		return nil, err
	}
	files, checkHash, err := prepareFiles(tor.Info, where)
	if err != nil {
		return nil, err
	}
	pieces := newPieces(tor.Info, files)
	bitField := bitfield.New(uint32(len(pieces)))
	if checkHash {
		for _, p := range pieces {
			ok, err := p.hashCheck()
			if err != nil {
				return nil, err
			}
			if ok {
				bitField.Set(p.index)
			}
		}
	}
	name := tor.Info.Name
	if len(name) > 8 {
		name = name[:8]
	}
	return &transfer{
		rain:     r,
		tracker:  tracker,
		torrent:  tor,
		pieces:   pieces,
		bitField: bitField,
		Finished: make(chan struct{}),
		haveC:    make(chan peerHave),
		peers:    make(map[*peer]struct{}),
		log:      logger.New("download " + name),
	}, nil
}

func (t *transfer) InfoHash() protocol.InfoHash { return t.torrent.Info.Hash }
func (t *transfer) Downloaded() int64 {
	var sum int64
	for i := uint32(0); i < t.bitField.Len(); i++ {
		if t.bitField.Test(i) {
			sum += int64(t.pieces[i].length)
		}
	}
	return sum
}
func (t *transfer) Uploaded() int64 { return 0 } // TODO
func (t *transfer) Left() int64     { return t.torrent.Info.TotalLength - t.Downloaded() }

func (t *transfer) Run() {
	sKey := mse.HashSKey(t.torrent.Info.Hash[:])

	t.rain.transfersM.Lock()
	t.rain.transfers[t.torrent.Info.Hash] = t
	t.rain.transfersSKey[sKey] = t
	t.rain.transfersM.Unlock()

	defer func() {
		t.rain.transfersM.Lock()
		delete(t.rain.transfers, t.torrent.Info.Hash)
		delete(t.rain.transfersSKey, sKey)
		t.rain.transfersM.Unlock()
	}()

	announceC := make(chan *tracker.AnnounceResponse)
	if t.bitField.All() {
		go tracker.AnnouncePeriodically(t.tracker, t, nil, tracker.Completed, nil, announceC)
	} else {
		go tracker.AnnouncePeriodically(t.tracker, t, nil, tracker.Started, nil, announceC)
	}

	downloader := newDownloader(t)
	go downloader.Run()

	uploader := newUploader(t)
	go uploader.Run()

	for {
		select {
		case announceResponse := <-announceC:
			if announceResponse.Error != nil {
				t.log.Error(announceResponse.Error)
				break
			}
			t.log.Infof("Announce: %d seeder, %d leecher", announceResponse.Seeders, announceResponse.Leechers)
			downloader.peersC <- announceResponse.Peers
		case peerHave := <-t.haveC:
			piece := peerHave.piece
			piece.peersM.Lock()
			piece.peers = append(piece.peers, peerHave.peer)
			piece.peersM.Unlock()

			select {
			case downloader.haveNotifyC <- struct{}{}:
			default:
			}
		}
	}
}

func (t *transfer) connect(addr *net.TCPAddr) {
	conn, _, ext, _, err := connection.Dial(addr, !t.rain.config.Encryption.DisableOutgoing, t.rain.config.Encryption.ForceOutgoing, [8]byte{}, t.torrent.Info.Hash, t.rain.peerID)
	if err != nil {
		if err == connection.ErrOwnConnection {
			t.log.Debug(err)
		} else {
			t.log.Error(err)
		}
		return
	}
	defer conn.Close()
	p := newPeer(conn, outgoing)
	p.log.Info("Connected to peer")
	p.log.Debugf("Peer extensions: %s", ext)
	p.Serve(t)
}

func prepareFiles(info *torrent.Info, where string) (files []*os.File, checkHash bool, err error) {
	var f *os.File
	var exists bool

	if !info.MultiFile {
		f, exists, err = openOrAllocate(filepath.Join(where, info.Name), info.Length)
		if err != nil {
			return
		}
		if exists {
			checkHash = true
		}
		files = []*os.File{f}
		return
	}

	// Multiple files
	files = make([]*os.File, len(info.Files))
	for i, f := range info.Files {
		parts := append([]string{where, info.Name}, f.Path...)
		path := filepath.Join(parts...)
		err = os.MkdirAll(filepath.Dir(path), os.ModeDir|0755)
		if err != nil {
			return
		}
		files[i], exists, err = openOrAllocate(path, f.Length)
		if err != nil {
			return
		}
		if exists {
			checkHash = true
		}
	}
	return
}

func openOrAllocate(path string, length int64) (f *os.File, exists bool, err error) {
	f, err = os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0640)
	if err != nil {
		return
	}

	defer func() {
		if err != nil {
			f.Close()
		}
	}()

	fi, err := f.Stat()
	if err != nil {
		return
	}

	if fi.Size() == 0 && length != 0 {
		if err = f.Truncate(length); err != nil {
			return
		}
		if err = f.Sync(); err != nil {
			return
		}
	} else {
		if fi.Size() != length {
			err = fmt.Errorf("%s expected to be %d bytes but it is %d bytes", path, length, fi.Size())
			return
		}
		exists = true
	}

	return
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func minUint32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

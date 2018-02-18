package swarm

import (
	"sync"
	"time"
	"xd/lib/bittorrent"
	"xd/lib/common"
	"xd/lib/log"
	"xd/lib/storage"
)

// how big should we download pieces at a time (bytes)?
const BlockSize = 1024 * 16

// cached downloading piece
type cachedPiece struct {
	piece      common.PieceData
	pending    *bittorrent.Bitfield
	obtained   *bittorrent.Bitfield
	lastActive time.Time
	mtx        sync.Mutex
}

// is this piece done downloading ?
func (p *cachedPiece) done() bool {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	return p.obtained.Completed()
}

// put a slice of data at offset
func (p *cachedPiece) put(offset uint32, data []byte) {
	p.mtx.Lock()
	l := uint32(len(data))
	if offset+l <= uint32(len(p.piece.Data)) {
		// put data
		copy(p.piece.Data[offset:offset+l], data)
		// set obtained
		idx := offset / BlockSize
		if l != BlockSize {
			// last block of last piece
			idx++
		}
		p.obtained.Set(idx)
		p.pending.Unset(idx)
	} else {
		log.Warnf("block out of range %d, %d", offset, len(data))
	}
	p.lastActive = time.Now()
	p.mtx.Unlock()
}

// cancel a slice
func (p *cachedPiece) cancel(offset, length uint32) {
	p.mtx.Lock()
	idx := offset / BlockSize
	log.Debugf("cancel piece idx=%d offset=%d bit=%d", p.piece.Index, offset, idx)
	p.pending.Unset(idx)
	p.lastActive = time.Now()
	p.mtx.Unlock()
}

func (p *cachedPiece) nextRequest() (r *common.PieceRequest) {
	p.mtx.Lock()
	defer p.mtx.Unlock()
	l := uint32(len(p.piece.Data))
	r = new(common.PieceRequest)
	r.Index = p.piece.Index
	r.Length = BlockSize
	idx := uint32(0)
	for r.Begin < l {
		if p.pending.Has(idx) || p.obtained.Has(idx) {
			r.Begin += BlockSize
			idx++
		} else {
			break
		}
	}

	if r.Begin+r.Length > l {
		// is this probably the last piece ?
		if (r.Begin+r.Length)-l >= BlockSize {
			// no, let's just say there are no more blocks left
			log.Debugf("no next piece request for idx=%d", r.Index)
			r = nil
			return
		} else {
			// yes so let's correct the size
			r.Length = l - r.Begin
		}
	}
	log.Debugf("next piece request made: idx=%d offset=%d len=%d total=%d", r.Index, r.Begin, r.Length, l)
	p.pending.Set(idx)
	return
}

// picks the next good piece to download
type PiecePicker func(*bittorrent.Bitfield, []uint32) (uint32, bool)

type pieceTracker struct {
	mtx        sync.Mutex
	requests   map[uint32]*cachedPiece
	pending    int
	st         storage.Torrent
	have       func(uint32)
	nextPiece  PiecePicker
	maxPending int
}

func (pt *pieceTracker) visitCached(idx uint32, v func(*cachedPiece)) {
	pt.mtx.Lock()
	defer pt.mtx.Unlock()
	_, has := pt.requests[idx]
	if !has {
		if !pt.newPiece(idx) {
			return
		}
	}
	v(pt.requests[idx])
}

func createPieceTracker(st storage.Torrent, picker PiecePicker) (pt *pieceTracker) {
	pt = &pieceTracker{
		requests:   make(map[uint32]*cachedPiece),
		st:         st,
		nextPiece:  picker,
		maxPending: 8,
	}
	return
}

func (pt *pieceTracker) newPiece(piece uint32) bool {

	if pt.pending >= pt.maxPending {
		return false
	}

	info := pt.st.MetaInfo()

	sz := info.LengthOfPiece(piece)
	bits := sz / BlockSize
	log.Debugf("new piece idx=%d len=%d bits=%d", piece, sz, bits)
	pt.requests[piece] = &cachedPiece{
		pending:  bittorrent.NewBitfield(bits, nil),
		obtained: bittorrent.NewBitfield(bits, nil),
		piece: common.PieceData{
			Data:  make([]byte, sz),
			Index: piece,
		},
		lastActive: time.Now(),
	}
	pt.pending++
	return true
}

func (pt *pieceTracker) removePiece(piece uint32) {
	pt.mtx.Lock()
	delete(pt.requests, piece)
	pt.pending--
	pt.mtx.Unlock()
}

func (pt *pieceTracker) pendingPiece(remote *bittorrent.Bitfield) (idx uint32, old bool) {
	for k := range pt.requests {
		if remote.Has(k) {
			idx = k
			old = true
			break
		}
	}
	return
}

// cancel entire pieces that have not been fetched within a duration
func (pt *pieceTracker) cancelTimedOut(dlt time.Duration) {
	pt.mtx.Lock()

	now := time.Now()
	for idx := range pt.requests {
		if now.Sub(pt.requests[idx].lastActive) > dlt {
			delete(pt.requests, idx)
			pt.pending--
		}
	}
	pt.mtx.Unlock()
}

func (pt *pieceTracker) nextRequestForDownload(remote *bittorrent.Bitfield, req *common.PieceRequest) bool {
	var r *common.PieceRequest
	pt.cancelTimedOut(time.Second * 30)
	idx, old := pt.pendingPiece(remote)
	if old {
		pt.visitCached(idx, func(cp *cachedPiece) {
			r = cp.nextRequest()
		})
	}
	if r == nil {
		var exclude []uint32
		for k := range pt.requests {
			exclude = append(exclude, k)
		}
		log.Debugf("get next piece excluding %d", exclude)
		var has bool
		idx, has = pt.nextPiece(remote, exclude)
		if has {
			pt.visitCached(idx, func(cp *cachedPiece) {
				r = cp.nextRequest()
			})
		}
	}
	if r != nil && r.Length > 0 {
		req.Copy(r)
	} else {
		return false
	}
	return true
}

// cancel previously requested piece request
func (pt *pieceTracker) canceledRequest(r common.PieceRequest) {
	if r.Length == 0 {
		return
	}
	pt.visitCached(r.Index, func(pc *cachedPiece) {
		pc.cancel(r.Begin, r.Length)
	})
}

func (pt *pieceTracker) handlePieceData(d common.PieceData) {
	idx := d.Index
	pt.visitCached(idx, func(pc *cachedPiece) {
		begin := d.Begin
		data := d.Data
		pc.put(begin, data)
		if pc.done() {
			err := pt.st.PutPiece(pc.piece)
			if err == nil {
				pt.st.Flush()
				if pt.have != nil {
					pt.have(d.Index)
				}
			} else {
				log.Warnf("put piece %d failed: %s", pc.piece.Index, err)
			}
			pt.removePiece(d.Index)
		}
	})
}

package multiplex

import (
	"errors"
	"io"
	"net"
	"time"

	log "github.com/sirupsen/logrus"
	"math"
	prand "math/rand"
	"sync"
	"sync/atomic"
)

var ErrBrokenStream = errors.New("broken stream")

type ReadWriteCloseLener interface {
	io.ReadWriteCloser
	Len() int
}

type Stream struct {
	id uint32

	session *Session

	buf ReadWriteCloseLener

	sorter *frameSorter

	// atomic
	nextSendSeq uint32

	writingM sync.RWMutex

	closed uint32

	obfsBuf []byte

	// we assign each stream a fixed underlying TCP connection to utilise order guarantee provided by TCP itself
	// so that frameSorter should have few to none ooo frames to deal with
	// overall the streams in a session should be uniformly distributed across all connections
	assignedConnId uint32
}

func makeStream(sesh *Session, id uint32, assignedConnId uint32) *Stream {
	var buf ReadWriteCloseLener
	if sesh.Unordered {
		buf = NewDatagramBuffer()
	} else {
		buf = NewBufferedPipe()
	}

	stream := &Stream{
		id:             id,
		session:        sesh,
		buf:            buf,
		obfsBuf:        make([]byte, 17000),
		sorter:         NewFrameSorter(buf),
		assignedConnId: assignedConnId,
	}

	return stream
}

//func (s *Stream) reassignConnId(connId uint32) { atomic.StoreUint32(&s.assignedConnId,connId)}

func (s *Stream) isClosed() bool { return atomic.LoadUint32(&s.closed) == 1 }

func (s *Stream) writeFrame(frame *Frame) {
	if s.session.Unordered {
		s.buf.Write(frame.Payload)
	} else {
		s.sorter.writeNewFrame(frame)
	}
}

func (s *Stream) Read(buf []byte) (n int, err error) {
	//log.Tracef("attempting to read from stream %v", s.id)
	if len(buf) == 0 {
		if s.isClosed() {
			return 0, ErrBrokenStream
		} else {
			return 0, nil
		}
	}

	if s.isClosed() {
		// TODO: Len check may not be necessary as this can be offloaded to buffer implementation
		if s.buf.Len() == 0 {
			return 0, ErrBrokenStream
		} else {
			n, err = s.buf.Read(buf)
			log.Tracef("%v read from stream %v with err %v", n, s.id, err)
			return
		}
	} else {
		n, err = s.buf.Read(buf)
		log.Tracef("%v read from stream %v with err %v", n, s.id, err)
		return
	}
}

func (s *Stream) Write(in []byte) (n int, err error) {
	// RWMutex used here isn't really for RW.
	// we use it to exploit the fact that RLock doesn't create contention.
	// The use of RWMutex is so that the stream will not actively close
	// in the middle of the execution of Write. This may cause the closing frame
	// to be sent before the data frame and cause loss of packet.
	//log.Tracef("attempting to write %v bytes to stream %v",len(in),s.id)
	s.writingM.RLock()
	defer s.writingM.RUnlock()
	if s.isClosed() {
		return 0, ErrBrokenStream
	}

	f := &Frame{
		StreamID: s.id,
		Seq:      atomic.AddUint32(&s.nextSendSeq, 1) - 1,
		Closing:  0,
		Payload:  in,
	}

	i, err := s.session.Obfs(f, s.obfsBuf)
	if err != nil {
		return i, err
	}
	n, err = s.session.sb.send(s.obfsBuf[:i], &s.assignedConnId)
	log.Tracef("%v sent to remote through stream %v with err %v", len(in), s.id, err)
	if err != nil {
		return
	}
	return len(in), nil

}

// the necessary steps to mark the stream as closed and to release resources
func (s *Stream) _close() {
	atomic.StoreUint32(&s.closed, 1)
	s.sorter.Close() // this will trigger frameSorter to return
	s.buf.Close()
}

// only close locally. Used when the stream close is notified by the remote
func (s *Stream) passiveClose() {
	s._close()
	s.session.delStream(s.id)
	log.Tracef("stream %v passively closed", s.id)
}

// active close. Close locally and tell the remote that this stream is being closed
func (s *Stream) Close() error {

	s.writingM.Lock()
	defer s.writingM.Unlock()
	if s.isClosed() {
		return errors.New("Already Closed")
	}

	// Notify remote that this stream is closed
	prand.Seed(int64(s.id))
	padLen := int(math.Floor(prand.Float64()*200 + 300))
	pad := make([]byte, padLen)
	prand.Read(pad)
	f := &Frame{
		StreamID: s.id,
		Seq:      atomic.AddUint32(&s.nextSendSeq, 1) - 1,
		Closing:  1,
		Payload:  pad,
	}
	i, err := s.session.Obfs(f, s.obfsBuf)
	if err != nil {
		return err
	}
	_, err = s.session.sb.send(s.obfsBuf[:i], &s.assignedConnId)
	if err != nil {
		return err
	}

	s._close()
	s.session.delStream(s.id)
	log.Tracef("stream %v actively closed", s.id)
	return nil
}

// Same as passiveClose() but no call to session.delStream.
// This is called in session.Close() to avoid mutex deadlock
// We don't notify the remote because session.Close() is always
// called when the session is passively closed
func (s *Stream) closeNoDelMap() {
	log.Tracef("stream %v closed by session", s.id)
	s._close()
}

// the following functions are purely for implementing net.Conn interface.
// they are not used
var errNotImplemented = errors.New("Not implemented")

func (s *Stream) LocalAddr() net.Addr  { return s.session.addrs.Load().([]net.Addr)[0] }
func (s *Stream) RemoteAddr() net.Addr { return s.session.addrs.Load().([]net.Addr)[1] }

// TODO: implement the following
func (s *Stream) SetDeadline(t time.Time) error      { return errNotImplemented }
func (s *Stream) SetReadDeadline(t time.Time) error  { return errNotImplemented }
func (s *Stream) SetWriteDeadline(t time.Time) error { return errNotImplemented }

package sockjs

import (
	"errors"
	"net/http"
	"sync"
	"time"
)

type sessionState uint32

const (
	// brand new session, need to send "h" to receiver
	sessionOpening sessionState = iota
	// active session
	sessionActive
	// session being closed, sending "closeFrame" to receivers
	sessionClosing
	// closed session, no activity at all, should be removed from handler completely and not reused
	sessionClosed
)

var (
	// ErrSessionNotOpen error is used to denote session not in open state.
	// Recv() and Send() operations are not suppored if session is closed.
	ErrSessionNotOpen          = errors.New("sockjs: session not in open state")
	errSessionReceiverAttached = errors.New("sockjs: another receiver already attached")
)

type session struct {
	sync.RWMutex
	id    string
	req   *http.Request
	state SessionState

	recv       receiver       // protocol dependent receiver (xhr, eventsource, ...)
	sendBuffer []string       // messages to be sent to client
	recvBuffer *messageBuffer // messages received from client to be consumed by application
	closeFrame string         // closeFrame to send after session is closed

	// do not use SockJS framing for raw websocket connections
	raw bool

	// internal timer used to handle session expiration if no receiver is attached, or heartbeats if recevier is attached
	sessionTimeoutInterval time.Duration
	heartbeatInterval      time.Duration
	timer                  *time.Timer
	// once the session timeouts this channel also closes
	closeCh chan struct{}
}

type receiver interface {
	// sendBulk send multiple data messages in frame frame in format: a["msg 1", "msg 2", ....]
	sendBulk(...string)
	// sendFrame sends given frame over the wire (with possible chunking depending on receiver)
	sendFrame(string)
	// close closes the receiver in a "done" way (idempotent)
	close()
	canSend() bool
	// done notification channel gets closed whenever receiver ends
	doneNotify() <-chan struct{}
	// interrupted channel gets closed whenever receiver is interrupted (i.e. http connection drops,...)
	interruptedNotify() <-chan struct{}
}

// Session is a central component that handles receiving and sending frames. It maintains internal state
func newSession(req *http.Request, sessionID string, sessionTimeoutInterval, heartbeatInterval time.Duration) *session {

	s := &session{
		id:  sessionID,
		req: req,
		sessionTimeoutInterval: sessionTimeoutInterval,
		heartbeatInterval:      heartbeatInterval,
		recvBuffer:             newMessageBuffer(),
		closeCh:                make(chan struct{}),
	}

	s.Lock() // "go test -race" complains if ommited, not sure why as no race can happen here
	s.timer = time.AfterFunc(sessionTimeoutInterval, s.close)
	s.Unlock()
	return s
}

func (s *session) sendMessage(msg string) error {
	s.Lock()
	defer s.Unlock()
	if s.state > sessionActive {
		return ErrSessionNotOpen
	}
	s.sendBuffer = append(s.sendBuffer, msg)
	if s.recv != nil && s.recv.canSend() {
		s.recv.sendBulk(s.sendBuffer...)
		s.sendBuffer = nil
	}
	return nil
}

func (s *session) attachReceiver(recv receiver) error {
	s.Lock()
	defer s.Unlock()
	if s.recv != nil {
		return errSessionReceiverAttached
	}
	s.recv = recv
	go func(r receiver) {
		select {
		case <-r.doneNotify():
			s.detachReceiver()
		case <-r.interruptedNotify():
			s.detachReceiver()
			s.close()
		}
	}(recv)

	if s.state == SessionClosing {
		if !s.raw {
			s.recv.sendFrame(s.closeFrame)
		}
		s.recv.close()
		return nil
	}
	if s.state == SessionOpening {
		if !s.raw {
			s.recv.sendFrame("o")
		}
		s.state = SessionActive
	}
	s.recv.sendBulk(s.sendBuffer...)
	s.sendBuffer = nil
	s.timer.Stop()
	if s.heartbeatInterval > 0 {
		s.timer = time.AfterFunc(s.heartbeatInterval, s.heartbeat)
	}
	return nil
}

func (s *session) detachReceiver() {
	s.Lock()
	s.timer.Stop()
	s.timer = time.AfterFunc(s.sessionTimeoutInterval, s.close)
	s.recv = nil
	s.Unlock()
}

func (s *session) heartbeat() {
	s.Lock()
	if s.recv != nil { // timer could have fired between Lock and timer.Stop in detachReceiver
		s.recv.sendFrame("h")
		s.timer = time.AfterFunc(s.heartbeatInterval, s.heartbeat)
	}
	s.Unlock()
}

func (s *session) accept(messages ...string) error {
	return s.recvBuffer.push(messages...)
}

// idempotent operation
func (s *session) closing() {
	s.Lock()
	defer s.Unlock()
	if s.state < SessionClosing {
		s.state = SessionClosing
		s.recvBuffer.close()
		if s.recv != nil {
			s.recv.sendFrame(s.closeFrame)
			s.recv.close()
		}
	}
}

// idempotent operation
func (s *session) close() {
	s.closing()
	s.Lock()
	defer s.Unlock()
	if s.state < sessionClosed {
		s.state = sessionClosed
		s.timer.Stop()
		close(s.closeCh)
	}
}

func (s *session) closedNotify() <-chan struct{} { return s.closeCh }

// Conn interface implementation
func (s *session) Close(status uint32, reason string) error {
	s.Lock()
	if s.state < sessionClosing {
		s.closeFrame = closeFrame(status, reason)
		s.Unlock()
		s.closing()
		return nil
	}
	s.Unlock()
	return ErrSessionNotOpen
}

func (s *session) Recv() (string, error) {
	return s.recvBuffer.pop()
}

func (s *session) Send(msg string) error {
	return s.sendMessage(msg)
}

func (s *session) ID() string { return s.id }

func (s *session) GetSessionState() SessionState {
	s.RLock()
	defer s.RUnlock()
	return s.state
}

func (s *session) Request() *http.Request {
	return s.req
}

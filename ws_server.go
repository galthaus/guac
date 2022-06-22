package guac

import (
	"bytes"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"gitlab.com/rackn/logger"
)

// WebsocketServer implements a websocket-based connection to guacd.
type WebsocketServer struct {
	logger logger.Logger

	connect func(*gin.Context) (Tunnel, error)

	// OnConnect is an optional callback called when a websocket connects.
	OnConnect func(string, string)
	// OnDisconnect is an optional callback called when the websocket disconnects.
	OnDisconnect func(string)
}

// NewWebsocketServer creates a new server with a connect method that takes a websocket.
func NewWebsocketServer(l logger.Logger, connect func(*gin.Context) (Tunnel, error)) *WebsocketServer {
	return &WebsocketServer{
		logger:  l,
		connect: connect,
	}
}

const (
	websocketReadBufferSize  = MaxGuacMessage
	websocketWriteBufferSize = MaxGuacMessage * 2
)

func (s *WebsocketServer) ServeHTTP(c *gin.Context) {
	w := c.Writer
	r := c.Request
	upgrader := websocket.Upgrader{
		ReadBufferSize:  websocketReadBufferSize,
		WriteBufferSize: websocketWriteBufferSize,
		CheckOrigin: func(r *http.Request) bool {
			return true // TODO
		},
	}
	protocol := r.Header.Get("Sec-Websocket-Protocol")
	ws, err := upgrader.Upgrade(w, r, http.Header{
		"Sec-Websocket-Protocol": {protocol},
	})
	if err != nil {
		s.logger.Errorf("Failed to upgrade websocket: %v", err)
		return
	}
	defer func() {
		if err = ws.Close(); err != nil {
			s.logger.Tracef("Error closing websocket: %v", err)
		}
	}()

	s.logger.Debugf("websocket: Connecting to tunnel")
	tunnel, e := s.connect(c)
	if e != nil {
		return
	}
	defer func() {
		if err = tunnel.Close(); err != nil {
			s.logger.Tracef("Error closing tunnel: %v", err)
		}
	}()
	s.logger.Debugf("websocket: Connected to tunnel")

	id := tunnel.ConnectionID()

	if s.OnConnect != nil {
		s.OnConnect(id, tunnel.Info())
	}

	writer := tunnel.AcquireWriter()
	reader := tunnel.AcquireReader()

	if s.OnDisconnect != nil {
		defer s.OnDisconnect(id)
	}

	defer tunnel.ReleaseWriter()
	defer tunnel.ReleaseReader()

	go s.wsToGuacd(ws, writer)
	s.guacdToWs(ws, reader)
}

// MessageReader wraps a websocket connection and only permits Reading
type MessageReader interface {
	// ReadMessage should return a single complete message to send to guac
	ReadMessage() (int, []byte, error)
}

func (s *WebsocketServer) wsToGuacd(ws MessageReader, guacd io.Writer) {
	for {
		_, data, err := ws.ReadMessage()
		if err != nil {
			s.logger.Tracef("Error reading message from ws: %v", err)
			return
		}

		if bytes.HasPrefix(data, internalOpcodeIns) {
			// messages starting with the InternalDataOpcode are never sent to guacd
			continue
		}

		if _, err = guacd.Write(data); err != nil {
			s.logger.Tracef("Failed writing to guacd: %v", err)
			return
		}
	}
}

// MessageWriter wraps a websocket connection and only permits Writing
type MessageWriter interface {
	// WriteMessage writes one or more complete guac commands to the websocket
	WriteMessage(int, []byte) error
}

func (s *WebsocketServer) guacdToWs(ws MessageWriter, guacd InstructionReader) {
	buf := bytes.NewBuffer(make([]byte, 0, MaxGuacMessage*2))

	for {
		ins, err := guacd.ReadSome()
		if err != nil {
			s.logger.Tracef("Error reading from guacd: %v", err)
			return
		}

		if bytes.HasPrefix(ins, internalOpcodeIns) {
			// messages starting with the InternalDataOpcode are never sent to the websocket
			continue
		}

		if _, err = buf.Write(ins); err != nil {
			s.logger.Tracef("Failed to buffer guacd to ws: %v", err)
			return
		}

		// if the buffer has more data in it or we've reached the max buffer size, send the data and reset
		if !guacd.Available() || buf.Len() >= MaxGuacMessage {
			if err = ws.WriteMessage(1, buf.Bytes()); err != nil {
				if err == websocket.ErrCloseSent {
					return
				}
				s.logger.Tracef("Failed sending message to ws: %v", err)
				return
			}
			buf.Reset()
		}
	}
}

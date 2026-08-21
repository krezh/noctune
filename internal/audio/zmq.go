// Minimal ZMTP 3.0 REQ client (NULL security mechanism only), just
// enough to talk to ffmpeg's azmq filter over a Unix socket and send it
// runtime filter commands (e.g. changing the volume filter's level
// without restarting the encoder). ffmpeg's ipc:// transport is a plain
// Unix domain socket, so no ZeroMQ library or transport code is needed —
// only the small, frozen wire protocol layered on top of it.
package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"time"
)

type zmqReqClient struct {
	conn net.Conn
}

// dialZMQReq connects to a ZMTP REP peer (e.g. ffmpeg's azmq filter)
// listening on a Unix socket and completes the ZMTP 3.0 greeting and
// NULL-mechanism READY handshake.
func dialZMQReq(socketPath string, timeout time.Duration) (*zmqReqClient, error) {
	conn, err := net.DialTimeout("unix", socketPath, timeout)
	if err != nil {
		return nil, fmt.Errorf("dial zmq socket: %w", err)
	}
	c := &zmqReqClient{conn: conn}
	if err := c.handshake(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return c, nil
}

func (c *zmqReqClient) handshake() error {
	greeting := make([]byte, 64)
	greeting[0] = 0xFF
	greeting[9] = 0x7F
	greeting[10] = 3 // version-major
	greeting[11] = 0 // version-minor
	copy(greeting[12:32], "NULL")
	if _, err := c.conn.Write(greeting); err != nil {
		return fmt.Errorf("send greeting: %w", err)
	}
	peerGreeting := make([]byte, 64)
	if _, err := io.ReadFull(c.conn, peerGreeting); err != nil {
		return fmt.Errorf("read greeting: %w", err)
	}
	if peerGreeting[0] != 0xFF || peerGreeting[9] != 0x7F {
		return fmt.Errorf("zmq: unexpected greeting signature")
	}

	if err := c.writeCommand(readyCommandBody("REQ")); err != nil {
		return fmt.Errorf("send READY: %w", err)
	}
	if _, _, err := c.readFrame(); err != nil {
		return fmt.Errorf("read peer READY: %w", err)
	}
	return nil
}

// readyCommandBody builds a ZMTP READY command body declaring this
// peer's socket type.
func readyCommandBody(socketType string) []byte {
	body := []byte{5}
	body = append(body, "READY"...)
	body = append(body, byte(len("Socket-Type")))
	body = append(body, "Socket-Type"...)
	valLen := make([]byte, 4)
	binary.BigEndian.PutUint32(valLen, uint32(len(socketType)))
	body = append(body, valLen...)
	body = append(body, socketType...)
	return body
}

const (
	flagMore    = 0x01
	flagLong    = 0x02
	flagCommand = 0x04
)

func (c *zmqReqClient) writeFrame(body []byte, more, command bool) error {
	var flags byte
	if more {
		flags |= flagMore
	}
	if command {
		flags |= flagCommand
	}
	var header []byte
	if len(body) < 256 {
		header = []byte{flags, byte(len(body))}
	} else {
		flags |= flagLong
		header = make([]byte, 9)
		header[0] = flags
		binary.BigEndian.PutUint64(header[1:], uint64(len(body)))
	}
	if _, err := c.conn.Write(header); err != nil {
		return err
	}
	_, err := c.conn.Write(body)
	return err
}

func (c *zmqReqClient) writeCommand(body []byte) error {
	return c.writeFrame(body, false, true)
}

// readFrame reads one ZMTP frame, returning its body and whether more
// frames follow in the same message.
func (c *zmqReqClient) readFrame() (body []byte, more bool, err error) {
	var flagByte [1]byte
	if _, err := io.ReadFull(c.conn, flagByte[:]); err != nil {
		return nil, false, err
	}
	flags := flagByte[0]
	var length uint64
	if flags&flagLong != 0 {
		var lenBuf [8]byte
		if _, err := io.ReadFull(c.conn, lenBuf[:]); err != nil {
			return nil, false, err
		}
		length = binary.BigEndian.Uint64(lenBuf[:])
	} else {
		var lenBuf [1]byte
		if _, err := io.ReadFull(c.conn, lenBuf[:]); err != nil {
			return nil, false, err
		}
		length = uint64(lenBuf[0])
	}
	body = make([]byte, length)
	if _, err := io.ReadFull(c.conn, body); err != nil {
		return nil, false, err
	}
	return body, flags&flagMore != 0, nil
}

// Send performs one REQ/REP exchange: msg is sent as a REQ envelope (an
// empty delimiter frame followed by the message frame), and the reply's
// message frame is returned.
func (c *zmqReqClient) Send(msg string) (string, error) {
	if err := c.writeFrame(nil, true, false); err != nil {
		return "", fmt.Errorf("send delimiter: %w", err)
	}
	if err := c.writeFrame([]byte(msg), false, false); err != nil {
		return "", fmt.Errorf("send message: %w", err)
	}

	if _, more, err := c.readFrame(); err != nil {
		return "", fmt.Errorf("read reply delimiter: %w", err)
	} else if !more {
		return "", fmt.Errorf("zmq: reply missing delimiter frame")
	}
	reply, _, err := c.readFrame()
	if err != nil {
		return "", fmt.Errorf("read reply: %w", err)
	}
	return string(reply), nil
}

func (c *zmqReqClient) Close() error {
	return c.conn.Close()
}

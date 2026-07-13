package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/google/uuid"

	"termcall/internal/protocol"
)

type wireMessage struct {
	Kind     string                 `json:"kind"`
	CallID   string                 `json:"call_id,omitempty"`
	Username string                 `json:"username,omitempty"`
	STUNURLs []string               `json:"stun_urls,omitempty"`
	Invite   protocol.SignalMessage `json:"invite,omitempty"`
	Signal   protocol.SignalMessage `json:"signal,omitempty"`
	Error    string                 `json:"error,omitempty"`
}

// Client is the interactive process's signaling connection to the user's
// daemon. Only signaling metadata crosses this local socket; media remains P2P.
type Client struct {
	username  string
	stunURLs  []string
	invite    protocol.SignalMessage
	conn      net.Conn
	encoder   *json.Encoder
	validator protocol.Validator

	ctx       context.Context
	cancel    context.CancelFunc
	events    chan protocol.SignalMessage
	errors    chan error
	writeMu   sync.Mutex
	closeOnce sync.Once
}

func Connect(ctx context.Context, socketPath, callID string) (*Client, error) {
	if socketPath == "" || callID == "" {
		return nil, errors.New("daemon socket path and call ID are required")
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("connect to termcall daemon: %w", err)
	}
	encoder, decoder := json.NewEncoder(connection), json.NewDecoder(connection)
	if deadline, ok := ctx.Deadline(); ok {
		_ = connection.SetDeadline(deadline)
	} else {
		_ = connection.SetDeadline(time.Now().Add(10 * time.Second))
	}
	if err := encoder.Encode(wireMessage{Kind: "attach", CallID: callID}); err != nil {
		connection.Close()
		return nil, err
	}
	var ready wireMessage
	if err := decoder.Decode(&ready); err != nil {
		connection.Close()
		return nil, fmt.Errorf("read daemon handoff: %w", err)
	}
	_ = connection.SetDeadline(time.Time{})
	if ready.Kind == "error" {
		connection.Close()
		return nil, errors.New(ready.Error)
	}
	if ready.Kind != "ready" || ready.Invite.CallID != callID || ready.Username == "" {
		connection.Close()
		return nil, errors.New("daemon returned an invalid call handoff")
	}
	clientContext, cancel := context.WithCancel(ctx)
	client := &Client{
		username: ready.Username, stunURLs: append([]string(nil), ready.STUNURLs...), invite: ready.Invite,
		conn: connection, encoder: encoder, validator: protocol.NewValidator(),
		ctx: clientContext, cancel: cancel, events: make(chan protocol.SignalMessage, 64), errors: make(chan error, 1),
	}
	go client.readLoop(decoder)
	go func() {
		<-clientContext.Done()
		client.Close()
	}()
	return client, nil
}

func (c *Client) Invite() protocol.SignalMessage        { return c.invite }
func (c *Client) Username() string                      { return c.username }
func (c *Client) STUNURLs() []string                    { return append([]string(nil), c.stunURLs...) }
func (c *Client) Events() <-chan protocol.SignalMessage { return c.events }
func (c *Client) Errors() <-chan error                  { return c.errors }

func (c *Client) NewMessage(messageType protocol.SignalType, to, callID string, payload any) (protocol.SignalMessage, error) {
	message := protocol.SignalMessage{
		Version: protocol.ProtocolVersion, ID: uuid.NewString(), Type: messageType,
		Timestamp: time.Now().UTC(), CallID: callID, From: c.username, To: to,
	}
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return protocol.SignalMessage{}, err
		}
		message.Payload = encoded
	}
	if err := c.validator.ValidateSignal(message); err != nil {
		return protocol.SignalMessage{}, err
	}
	return message, nil
}

func (c *Client) Send(ctx context.Context, message protocol.SignalMessage) error {
	if err := c.validator.ValidateSignal(message); err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := c.encoder.Encode(wireMessage{Kind: "signal", Signal: message}); err != nil {
		return fmt.Errorf("send signal through daemon: %w", err)
	}
	return nil
}

func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.conn.Close()
	})
}

func (c *Client) readLoop(decoder *json.Decoder) {
	defer close(c.events)
	for {
		var message wireMessage
		if err := decoder.Decode(&message); err != nil {
			if c.ctx.Err() == nil {
				c.report(fmt.Errorf("daemon signaling handoff closed: %w", err))
			}
			c.Close()
			return
		}
		switch message.Kind {
		case "signal":
			if err := c.validator.ValidateSignal(message.Signal); err != nil {
				c.report(err)
				continue
			}
			select {
			case c.events <- message.Signal:
			case <-c.ctx.Done():
				return
			}
		case "error":
			c.report(errors.New(message.Error))
		}
	}
}

func (c *Client) report(err error) {
	select {
	case c.errors <- err:
	default:
	}
}

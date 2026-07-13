package signaling

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"

	"termcall/internal/identity"
	"termcall/internal/protocol"
)

var (
	ErrClosed          = errors.New("signaling client is closed")
	ErrUnexpectedHello = errors.New("unexpected signaling handshake response")
)

type Config struct {
	URL              string
	Address          string
	QueueSize        int
	HandshakeTimeout time.Duration
	WriteTimeout     time.Duration
	AccessKey        string
	Identity         *identity.Identity
}

type Client struct {
	address   string
	identity  *identity.Identity
	conn      *websocket.Conn
	validator protocol.Validator
	writeWait time.Duration
	stunURLs  []string

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	events   chan protocol.SignalMessage
	errors   chan error
	outbound chan outboundMessage

	closeOnce sync.Once
	wg        sync.WaitGroup
}

type outboundMessage struct {
	data   []byte
	result chan error
}

func Connect(ctx context.Context, config Config) (*Client, error) {
	if !protocol.ValidAddress(config.Address) {
		return nil, fmt.Errorf("invalid canonical address %q", config.Address)
	}
	if config.Identity == nil || !identity.AddressMatchesKey(config.Address, config.Identity.PublicKey) {
		return nil, errors.New("canonical address requires its matching local identity")
	}
	if config.URL == "" {
		return nil, errors.New("signaling URL is required")
	}
	parsedURL, err := url.Parse(config.URL)
	if err != nil || (parsedURL.Scheme != "ws" && parsedURL.Scheme != "wss") || parsedURL.Host == "" {
		return nil, errors.New("signaling URL must be a valid ws:// or wss:// URL")
	}
	if config.AccessKey != "" && parsedURL.Scheme == "ws" {
		hostname := parsedURL.Hostname()
		if hostname != "localhost" && hostname != "127.0.0.1" && hostname != "::1" {
			return nil, errors.New("authenticated signaling requires wss:// for non-local servers")
		}
	}
	if config.QueueSize <= 0 {
		config.QueueSize = 64
	}
	if config.HandshakeTimeout <= 0 {
		config.HandshakeTimeout = 10 * time.Second
	}
	if config.WriteTimeout <= 0 {
		config.WriteTimeout = 5 * time.Second
	}
	if ctx == nil {
		ctx = context.Background()
	}
	clientContext, cancel := context.WithCancel(ctx)
	handshakeContext, handshakeCancel := context.WithTimeout(clientContext, config.HandshakeTimeout)
	defer handshakeCancel()
	dialOptions := &websocket.DialOptions{}
	if config.AccessKey != "" {
		dialOptions.HTTPHeader = http.Header{"Authorization": []string{"Bearer " + config.AccessKey}}
	}
	conn, _, err := websocket.Dial(handshakeContext, config.URL, dialOptions)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("connect signaling WebSocket: %w", err)
	}
	conn.SetReadLimit(protocol.MaxSDPMessageSize)

	validator := protocol.NewValidator()
	client := &Client{
		address: config.Address, identity: config.Identity, conn: conn, validator: validator, writeWait: config.WriteTimeout,
		ctx: clientContext, cancel: cancel, done: make(chan struct{}),
		events: make(chan protocol.SignalMessage, config.QueueSize), errors: make(chan error, 1),
		outbound: make(chan outboundMessage, config.QueueSize),
	}
	hello, err := client.NewMessage(protocol.SignalSessionHello, "", "", nil)
	if err != nil {
		client.Close()
		return nil, err
	}
	encoded, err := json.Marshal(hello)
	if err != nil {
		client.Close()
		return nil, err
	}
	if err := conn.Write(handshakeContext, websocket.MessageText, encoded); err != nil {
		client.Close()
		return nil, fmt.Errorf("send signaling hello: %w", err)
	}
	messageType, data, err := conn.Read(handshakeContext)
	if err != nil {
		client.Close()
		return nil, fmt.Errorf("read signaling ready: %w", err)
	}
	ready, err := protocol.DecodeSignal(data, validator)
	if err != nil || messageType != websocket.MessageText || ready.Type != protocol.SignalSessionReady || ready.To != config.Address {
		client.Close()
		return nil, fmt.Errorf("%w: %v", ErrUnexpectedHello, err)
	}
	if len(ready.Payload) != 0 {
		var payload protocol.SessionReadyPayload
		if err := json.Unmarshal(ready.Payload, &payload); err != nil {
			client.Close()
			return nil, fmt.Errorf("%w: invalid session configuration", ErrUnexpectedHello)
		}
		client.stunURLs = append([]string(nil), payload.STUNURLs...)
	}

	client.wg.Add(2)
	go client.readLoop()
	go client.writeLoop()
	go func() {
		client.wg.Wait()
		close(client.done)
	}()
	go func() {
		<-clientContext.Done()
		client.Close()
	}()
	return client, nil
}

func (c *Client) NewMessage(messageType protocol.SignalType, to, callID string, payload any) (protocol.SignalMessage, error) {
	message := protocol.SignalMessage{
		Version: protocol.ProtocolVersion, ID: uuid.NewString(), Type: messageType,
		Timestamp: time.Now().UTC(), CallID: callID, From: c.address, To: to,
	}
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return protocol.SignalMessage{}, fmt.Errorf("encode signaling payload: %w", err)
		}
		message.Payload = encoded
	}
	if payload == nil && (messageType == protocol.SignalSessionHello || messageType == protocol.SignalCallInvite || messageType == protocol.SignalCallAccept) {
		proof, err := c.identity.Sign(messageType, callID, c.address, to, time.Now())
		if err != nil {
			return protocol.SignalMessage{}, err
		}
		message.Payload, err = json.Marshal(proof)
		if err != nil {
			return protocol.SignalMessage{}, err
		}
	}
	if err := c.validator.ValidateSignal(message); err != nil {
		return protocol.SignalMessage{}, err
	}
	return message, nil
}

func (c *Client) Send(ctx context.Context, message protocol.SignalMessage) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.validator.ValidateSignal(message); err != nil {
		return err
	}
	encoded, err := json.Marshal(message)
	if err != nil {
		return fmt.Errorf("encode signaling message: %w", err)
	}
	result := make(chan error, 1)
	select {
	case c.outbound <- outboundMessage{data: encoded, result: result}:
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return ErrClosed
	}
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return ErrClosed
	}
}

func (c *Client) Events() <-chan protocol.SignalMessage { return c.events }
func (c *Client) Errors() <-chan error                  { return c.errors }
func (c *Client) Done() <-chan struct{}                 { return c.done }
func (c *Client) STUNURLs() []string                    { return append([]string(nil), c.stunURLs...) }

func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.cancel()
		_ = c.conn.CloseNow()
	})
}

func (c *Client) readLoop() {
	defer c.wg.Done()
	for {
		messageType, data, err := c.conn.Read(c.ctx)
		if err != nil {
			if c.ctx.Err() == nil {
				c.report(fmt.Errorf("read signaling message: %w", err))
			}
			c.Close()
			return
		}
		if messageType != websocket.MessageText {
			continue
		}
		message, err := protocol.DecodeSignal(data, c.validator)
		if err != nil {
			c.report(err)
			continue
		}
		if message.Type == protocol.SignalSessionPing {
			pong, createErr := c.NewMessage(protocol.SignalSessionPong, "server", "", nil)
			if createErr != nil || c.Send(c.ctx, pong) != nil {
				c.Close()
				return
			}
			continue
		}
		select {
		case c.events <- message:
		case <-c.ctx.Done():
			return
		}
	}
}

func (c *Client) writeLoop() {
	defer c.wg.Done()
	for {
		select {
		case <-c.ctx.Done():
			return
		case outbound := <-c.outbound:
			writeContext, cancel := context.WithTimeout(c.ctx, c.writeWait)
			err := c.conn.Write(writeContext, websocket.MessageText, outbound.data)
			cancel()
			outbound.result <- err
			if err != nil {
				if c.ctx.Err() == nil {
					c.report(fmt.Errorf("write signaling message: %w", err))
				}
				c.Close()
				return
			}
		}
	}
}

func (c *Client) report(err error) {
	select {
	case c.errors <- err:
	default:
	}
}

// Copyright (c) 2021 Tulir Asokan
//
// This Source Code Form is subject to the terms of the Mozilla Public
// License, v. 2.0. If a copy of the MPL was not distributed with this
// file, You can obtain one at http://mozilla.org/MPL/2.0/.

// Package whatsmeow implements a client for interacting with the WhatsApp web multidevice API.
package whatsmeow

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-whatsapp/go-util/random"
	"github.com/puzpuzpuz/xsync/v3"

	"github.com/go-whatsapp/whatsmeow/appstate"
	waBinary "github.com/go-whatsapp/whatsmeow/binary"
	waProto "github.com/go-whatsapp/whatsmeow/binary/proto"
	"github.com/go-whatsapp/whatsmeow/socket"
	"github.com/go-whatsapp/whatsmeow/store"
	"github.com/go-whatsapp/whatsmeow/types"
	"github.com/go-whatsapp/whatsmeow/types/events"
	"github.com/go-whatsapp/whatsmeow/util/keys"
	waLog "github.com/go-whatsapp/whatsmeow/util/log"
)

// EventHandler is a function that can handle events from WhatsApp.
type EventHandler func(evt interface{})
type nodeHandler func(node *waBinary.Node)

var nextHandlerID uint32

type wrappedEventHandler struct {
	fn EventHandler
	id uint32
}

// Client contains everything necessary to connect to and interact with the WhatsApp web API.
type Client struct {
	Store   *store.Device
	Log     waLog.Logger
	recvLog waLog.Logger
	sendLog waLog.Logger

	socket     *socket.NoiseSocket
	socketLock xsync.RBMutex
	socketWait chan struct{}

	isLoggedIn            uint32
	expectedDisconnectVal uint32
	EnableAutoReconnect   bool
	LastSuccessfulConnect time.Time
	AutoReconnectErrors   int
	// AutoReconnectHook is called when auto-reconnection fails. If the function returns false,
	// the client will not attempt to reconnect. The number of retries can be read from AutoReconnectErrors.
	AutoReconnectHook func(error) bool

	sendActiveReceipts uint32

	// EmitAppStateEventsOnFullSync can be set to true if you want to get app state events emitted
	// even when re-syncing the whole state.
	EmitAppStateEventsOnFullSync bool

	AutomaticMessageRerequestFromPhone bool
	pendingPhoneRerequests             *xsync.MapOf[types.MessageID, context.CancelFunc]

	appStateProc     *appstate.Processor
	appStateSyncLock sync.Mutex

	historySyncNotifications  chan *waProto.HistorySyncNotification
	historySyncHandlerStarted uint32

	uploadPreKeysLock sync.Mutex
	lastPreKeyUpload  time.Time

	mediaConnCache *MediaConn
	mediaConnLock  sync.Mutex

	responseWaiters *xsync.MapOf[string, chan<- *waBinary.Node]

	nodeHandlers      *xsync.MapOf[string, nodeHandler]
	handlerQueue      chan *waBinary.Node
	eventHandlers     []wrappedEventHandler
	eventHandlersLock xsync.RBMutex

	messageRetries *xsync.MapOf[string, int]

	incomingRetryRequestCounter *xsync.MapOf[incomingRetryKey, int]

	appStateKeyRequests *xsync.MapOf[string, time.Time]

	messageSendLock sync.Mutex

	privacySettingsCache atomic.Value

	groupParticipantsCache *xsync.MapOf[types.JID, []types.JID]
	userDevicesCache       *xsync.MapOf[types.JID, []types.JID]

	recentMessagesMap  *xsync.MapOf[recentMessageKey, *waProto.Message]
	recentMessagesList [recentMessagesSize]recentMessageKey
	recentMessagesPtr  int

	sessionRecreateHistory *xsync.MapOf[types.JID, time.Time]
	// GetMessageForRetry is used to find the source message for handling retry receipts
	// when the message is not found in the recently sent message cache.
	GetMessageForRetry func(requester, to types.JID, id types.MessageID) *waProto.Message
	// PreRetryCallback is called before a retry receipt is accepted.
	// If it returns false, the accepting will be cancelled and the retry receipt will be ignored.
	PreRetryCallback func(receipt *events.Receipt, id types.MessageID, retryCount int, msg *waProto.Message) bool

	// PrePairCallback is called before pairing is completed. If it returns false, the pairing will be cancelled and
	// the client will disconnect.
	PrePairCallback func(jid types.JID, platform, businessName string) bool

	// Should untrusted identity errors be handled automatically? If true, the stored identity and existing signal
	// sessions will be removed on untrusted identity errors, and an events.IdentityChange will be dispatched.
	// If false, decrypting a message from untrusted devices will fail.
	AutoTrustIdentity bool

	// Should sending to own devices be skipped when sending broadcasts?
	// This works around a bug in the WhatsApp android app where it crashes if you send a status message from a linked device.
	DontSendSelfBroadcast bool

	// Should SubscribePresence return an error if no privacy token is stored for the user?
	ErrorOnSubscribePresenceWithoutToken bool

	phoneLinkingCache *phoneLinkingCache

	uniqueID  string
	idCounter uint32

	proxy socket.Proxy
	http  *http.Client
}

// Size of buffer for the channel that all incoming XML nodes go through.
// In general it shouldn't go past a few buffered messages, but the channel is big to be safe.
const handlerQueueSize = 2048

// NewClient initializes a new WhatsApp web client.
//
// The logger can be nil, it will default to a no-op logger.
//
// The device store must be set. A default SQL-backed implementation is available in the store/sqlstore package.
//
//	container, err := sqlstore.New("sqlite3", "file:yoursqlitefile.db?_foreign_keys=on", nil)
//	if err != nil {
//		panic(err)
//	}
//	// If you want multiple sessions, remember their JIDs and use .GetDevice(jid) or .GetAllDevices() instead.
//	deviceStore, err := container.GetFirstDevice()
//	if err != nil {
//		panic(err)
//	}
//	client := whatsmeow.NewClient(deviceStore, nil)
func NewClient(deviceStore *store.Device, log waLog.Logger) *Client {
	if log == nil {
		log = waLog.Noop
	}
	uniqueIDPrefix := random.Bytes(2)
	cli := &Client{
		http: &http.Client{
			Transport: (http.DefaultTransport.(*http.Transport)).Clone(),
		},
		proxy:           http.ProxyFromEnvironment,
		Store:           deviceStore,
		Log:             log,
		recvLog:         log.Sub("Recv"),
		sendLog:         log.Sub("Send"),
		uniqueID:        fmt.Sprintf("%d.%d-", uniqueIDPrefix[0], uniqueIDPrefix[1]),
		responseWaiters: xsync.NewMapOf[string, chan<- *waBinary.Node](),
		eventHandlers:   make([]wrappedEventHandler, 0, 1),
		messageRetries:  xsync.NewMapOf[string, int](),
		nodeHandlers:    xsync.NewMapOfPresized[string, nodeHandler](11),
		handlerQueue:    make(chan *waBinary.Node, handlerQueueSize),
		appStateProc:    appstate.NewProcessor(deviceStore, log.Sub("AppState")),
		socketWait:      make(chan struct{}),

		incomingRetryRequestCounter: xsync.NewMapOf[incomingRetryKey, int](),

		historySyncNotifications: make(chan *waProto.HistorySyncNotification, 32),

		groupParticipantsCache: xsync.NewMapOf[types.JID, []types.JID](),
		userDevicesCache:       xsync.NewMapOf[types.JID, []types.JID](),

		recentMessagesMap:      xsync.NewMapOfPresized[recentMessageKey, *waProto.Message](recentMessagesSize),
		sessionRecreateHistory: xsync.NewMapOf[types.JID, time.Time](),
		GetMessageForRetry:     func(requester, to types.JID, id types.MessageID) *waProto.Message { return nil },
		appStateKeyRequests:    xsync.NewMapOf[string, time.Time](),

		pendingPhoneRerequests: xsync.NewMapOf[types.MessageID, context.CancelFunc](),

		EnableAutoReconnect:   true,
		AutoTrustIdentity:     true,
		DontSendSelfBroadcast: true,
	}
	cli.nodeHandlers.Store("message", cli.handleEncryptedMessage)
	cli.nodeHandlers.Store("receipt", cli.handleReceipt)
	cli.nodeHandlers.Store("call", cli.handleCallEvent)
	cli.nodeHandlers.Store("chatstate", cli.handleChatState)
	cli.nodeHandlers.Store("presence", cli.handlePresence)
	cli.nodeHandlers.Store("notification", cli.handleNotification)
	cli.nodeHandlers.Store("success", cli.handleConnectSuccess)
	cli.nodeHandlers.Store("failure", cli.handleConnectFailure)
	cli.nodeHandlers.Store("stream:error", cli.handleStreamError)
	cli.nodeHandlers.Store("iq", cli.handleIQ)
	cli.nodeHandlers.Store("ib", cli.handleIB)
	// Apparently there's also an <error> node which can have a code=479 and means "Invalid stanza sent (smax-invalid)"
	return cli
}

// SetProxyAddress is a helper method that parses a URL string and calls SetProxy.
//
// Returns an error if url.Parse fails to parse the given address.
func (cli *Client) SetProxyAddress(addr string) error {
	parsed, err := url.Parse(addr)
	if err != nil {
		return err
	}
	cli.SetProxy(http.ProxyURL(parsed))
	return nil
}

// SetProxy sets the proxy to use for WhatsApp web websocket connections and media uploads/downloads.
//
// Must be called before Connect() to take effect in the websocket connection.
// If you want to change the proxy after connecting, you must call Disconnect() and then Connect() again manually.
//
// By default, the client will find the proxy from the https_proxy environment variable like Go's net/http does.
//
// To disable reading proxy info from environment variables, explicitly set the proxy to nil:
//
//	cli.SetProxy(nil)
//
// To use a different proxy for the websocket and media, pass a function that checks the request path or headers:
//
//	cli.SetProxy(func(r *http.Request) (*url.URL, error) {
//		if r.URL.Host == "web.whatsapp.com" && r.URL.Path == "/ws/chat" {
//			return websocketProxyURL, nil
//		} else {
//			return mediaProxyURL, nil
//		}
//	})
func (cli *Client) SetProxy(proxy socket.Proxy) {
	cli.proxy = proxy
	cli.http.Transport.(*http.Transport).Proxy = proxy
}

func (cli *Client) getSocketWaitChan() <-chan struct{} {
	t := cli.socketLock.RLock()
	ch := cli.socketWait
	cli.socketLock.RUnlock(t)
	return ch
}

func (cli *Client) closeSocketWaitChan() {
	cli.socketLock.Lock()
	close(cli.socketWait)
	cli.socketWait = make(chan struct{})
	cli.socketLock.Unlock()
}

func (cli *Client) getOwnJID() types.JID {
	id := cli.Store.JID
	if id == nil {
		return types.EmptyJID
	}
	return *id
}

func (cli *Client) WaitForConnection(timeout time.Duration) bool {
	timeoutChan := time.After(timeout)
	t := cli.socketLock.RLock()
	for cli.socket == nil || !cli.socket.IsConnected() || !cli.IsLoggedIn() {
		ch := cli.socketWait
		cli.socketLock.RUnlock(t)
		select {
		case <-ch:
		case <-timeoutChan:
			return false
		}
		t = cli.socketLock.RLock()
	}
	cli.socketLock.RUnlock(t)
	return true
}

// Connect connects the client to the WhatsApp web websocket. After connection, it will either
// authenticate if there's data in the device store, or emit a QREvent to set up a new link.
func (cli *Client) Connect() error {
	cli.socketLock.Lock()
	defer cli.socketLock.Unlock()
	if cli.socket != nil {
		if !cli.socket.IsConnected() {
			cli.unlockedDisconnect()
		} else {
			return ErrAlreadyConnected
		}
	}

	cli.resetExpectedDisconnect()
	fs := socket.NewFrameSocket(cli.Log.Sub("Socket"), socket.WAConnHeader, cli.proxy)
	if err := fs.Connect(); err != nil {
		fs.Close(0)
		return err
	} else if err = cli.doHandshake(fs, *keys.NewKeyPair()); err != nil {
		fs.Close(0)
		return fmt.Errorf("noise handshake failed: %w", err)
	}
	go cli.keepAliveLoop(cli.socket.Context())
	go cli.handlerQueueLoop(cli.socket.Context())
	return nil
}

// IsLoggedIn returns true after the client is successfully connected and authenticated on WhatsApp.
func (cli *Client) IsLoggedIn() bool {
	return atomic.LoadUint32(&cli.isLoggedIn) == 1
}

func (cli *Client) onDisconnect(ns *socket.NoiseSocket, remote bool) {
	ns.Stop(false)
	cli.socketLock.Lock()
	defer cli.socketLock.Unlock()
	if cli.socket == ns {
		cli.socket = nil
		cli.clearResponseWaiters(xmlStreamEndNode)
		if !cli.isExpectedDisconnect() && remote {
			cli.Log.Debugf("Emitting Disconnected event")
			go cli.dispatchEvent(&events.Disconnected{})
			go cli.autoReconnect()
		} else if remote {
			cli.Log.Debugf("OnDisconnect() called, but it was expected, so not emitting event")
		} else {
			cli.Log.Debugf("OnDisconnect() called after manual disconnection")
		}
	} else {
		cli.Log.Debugf("Ignoring OnDisconnect on different socket")
	}
}

func (cli *Client) expectDisconnect() {
	atomic.StoreUint32(&cli.expectedDisconnectVal, 1)
}

func (cli *Client) resetExpectedDisconnect() {
	atomic.StoreUint32(&cli.expectedDisconnectVal, 0)
}

func (cli *Client) isExpectedDisconnect() bool {
	return atomic.LoadUint32(&cli.expectedDisconnectVal) == 1
}

func (cli *Client) autoReconnect() {
	if !cli.EnableAutoReconnect || cli.Store.JID == nil {
		return
	}
	for {
		autoReconnectDelay := time.Duration(cli.AutoReconnectErrors) * 2 * time.Second
		cli.Log.Debugf("Automatically reconnecting after %v", autoReconnectDelay)
		cli.AutoReconnectErrors++
		time.Sleep(autoReconnectDelay)
		err := cli.Connect()
		if errors.Is(err, ErrAlreadyConnected) {
			cli.Log.Debugf("Connect() said we're already connected after autoreconnect sleep")
			return
		} else if err != nil {
			cli.Log.Errorf("Error reconnecting after autoreconnect sleep: %v", err)
			if cli.AutoReconnectHook != nil && !cli.AutoReconnectHook(err) {
				cli.Log.Debugf("AutoReconnectHook returned false, not reconnecting")
				return
			}
		} else {
			return
		}
	}
}

// IsConnected checks if the client is connected to the WhatsApp web websocket.
// Note that this doesn't check if the client is authenticated. See the IsLoggedIn field for that.
func (cli *Client) IsConnected() bool {
	t := cli.socketLock.RLock()
	connected := cli.socket != nil && cli.socket.IsConnected()
	cli.socketLock.RUnlock(t)
	return connected
}

// Disconnect disconnects from the WhatsApp web websocket.
//
// This will not emit any events, the Disconnected event is only used when the
// connection is closed by the server or a network error.
func (cli *Client) Disconnect() {
	if cli.socket == nil {
		return
	}
	cli.socketLock.Lock()
	cli.unlockedDisconnect()
	cli.socketLock.Unlock()
}

// Disconnect closes the websocket connection.
func (cli *Client) unlockedDisconnect() {
	if cli.socket != nil {
		cli.socket.Stop(true)
		cli.socket = nil
		cli.clearResponseWaiters(xmlStreamEndNode)
	}
}

// Logout sends a request to unlink the device, then disconnects from the websocket and deletes the local device store.
//
// If the logout request fails, the disconnection and local data deletion will not happen either.
// If an error is returned, but you want to force disconnect/clear data, call Client.Disconnect() and Client.Store.Delete() manually.
//
// Note that this will not emit any events. The LoggedOut event is only used for external logouts
// (triggered by the user from the main device or by WhatsApp servers).
func (cli *Client) Logout() error {
	ownID := cli.getOwnJID()
	if ownID.IsEmpty() {
		return ErrNotLoggedIn
	}
	_, err := cli.sendIQ(infoQuery{
		Namespace: "md",
		Type:      iqSet,
		To:        types.ServerJID,
		Content: []waBinary.Node{{
			Tag: "remove-companion-device",
			Attrs: waBinary.Attrs{
				"jid":    ownID,
				"reason": "user_initiated",
			},
		}},
	})
	if err != nil {
		return fmt.Errorf("error sending logout request: %w", err)
	}
	cli.Disconnect()
	err = cli.Store.Delete()
	if err != nil {
		return fmt.Errorf("error deleting data from store: %w", err)
	}
	return nil
}

// AddEventHandler registers a new function to receive all events emitted by this client.
//
// The returned integer is the event handler ID, which can be passed to RemoveEventHandler to remove it.
//
// All registered event handlers will receive all events. You should use a type switch statement to
// filter the events you want:
//
//	func myEventHandler(evt interface{}) {
//		switch v := evt.(type) {
//		case *events.Message:
//			fmt.Println("Received a message!")
//		case *events.Receipt:
//			fmt.Println("Received a receipt!")
//		}
//	}
//
// If you want to access the Client instance inside the event handler, the recommended way is to
// wrap the whole handler in another struct:
//
//	type MyClient struct {
//		WAClient *whatsmeow.Client
//		eventHandlerID uint32
//	}
//
//	func (mycli *MyClient) register() {
//		mycli.eventHandlerID = mycli.WAClient.AddEventHandler(mycli.myEventHandler)
//	}
//
//	func (mycli *MyClient) myEventHandler(evt interface{}) {
//		// Handle event and access mycli.WAClient
//	}
func (cli *Client) AddEventHandler(handler EventHandler) uint32 {
	nextID := atomic.AddUint32(&nextHandlerID, 1)
	cli.eventHandlersLock.Lock()
	cli.eventHandlers = append(cli.eventHandlers, wrappedEventHandler{handler, nextID})
	cli.eventHandlersLock.Unlock()
	return nextID
}

// RemoveEventHandler removes a previously registered event handler function.
// If the function with the given ID is found, this returns true.
//
// N.B. Do not run this directly from an event handler. That would cause a deadlock because the
// event dispatcher holds a read lock on the event handler list, and this method wants a write lock
// on the same list. Instead run it in a goroutine:
//
//	func (mycli *MyClient) myEventHandler(evt interface{}) {
//		if noLongerWantEvents {
//			go mycli.WAClient.RemoveEventHandler(mycli.eventHandlerID)
//		}
//	}
func (cli *Client) RemoveEventHandler(id uint32) bool {
	cli.eventHandlersLock.Lock()
	defer cli.eventHandlersLock.Unlock()
	for index := range cli.eventHandlers {
		if cli.eventHandlers[index].id == id {
			if index == 0 {
				cli.eventHandlers[0].fn = nil
				cli.eventHandlers = cli.eventHandlers[1:]
				return true
			} else if index < len(cli.eventHandlers)-1 {
				copy(cli.eventHandlers[index:], cli.eventHandlers[index+1:])
			}
			cli.eventHandlers[len(cli.eventHandlers)-1].fn = nil
			cli.eventHandlers = cli.eventHandlers[:len(cli.eventHandlers)-1]
			return true
		}
	}
	return false
}

// RemoveEventHandlers removes all event handlers that have been registered with AddEventHandler
func (cli *Client) RemoveEventHandlers() {
	cli.eventHandlersLock.Lock()
	cli.eventHandlers = make([]wrappedEventHandler, 0, 1)
	cli.eventHandlersLock.Unlock()
}

func (cli *Client) handleFrame(data []byte) {
	decompressed, err := waBinary.Unpack(data)
	if err != nil {
		cli.Log.Warnf("Failed to decompress frame: %v", err)
		cli.Log.Debugf("Errored frame hex: %s", hex.EncodeToString(data))
		return
	}
	node, err := waBinary.Unmarshal(decompressed)
	if err != nil {
		cli.Log.Warnf("Failed to decode node in frame: %v", err)
		cli.Log.Debugf("Errored frame hex: %s", hex.EncodeToString(decompressed))
		return
	}
	cli.recvLog.Debugf("%s", node.XMLString())
	if node.Tag == "xmlstreamend" {
		if !cli.isExpectedDisconnect() {
			cli.Log.Warnf("Received stream end frame")
		}
		// TODO should we do something else?
	} else if cli.receiveResponse(node) {
		// handled
	} else if _, ok := cli.nodeHandlers.Load(node.Tag); ok {
		select {
		case cli.handlerQueue <- node:
		default:
			cli.Log.Warnf("Handler queue is full, message ordering is no longer guaranteed")
			go func() {
				cli.handlerQueue <- node
			}()
		}
	} else if node.Tag != "ack" {
		cli.Log.Debugf("Didn't handle WhatsApp node %s", node.Tag)
	}
}

func stopAndDrainTimer(timer *time.Timer) {
	if !timer.Stop() {
		select {
		case <-timer.C:
		default:
		}
	}
}

func (cli *Client) handlerQueueLoop(ctx context.Context) {
	timer := time.NewTimer(5 * time.Minute)
	stopAndDrainTimer(timer)
	cli.Log.Debugf("Starting handler queue loop")
	for {
		select {
		case node := <-cli.handlerQueue:
			doneChan := make(chan struct{}, 1)
			go func() {
				start := time.Now()
				f, ok := cli.nodeHandlers.Load(node.Tag)
				if ok {
					f(node)
				}
				duration := time.Since(start)
				doneChan <- struct{}{}
				if duration > 5*time.Second {
					cli.Log.Warnf("Node handling took %s for %s", duration, node.XMLString())
				}
			}()
			timer.Reset(5 * time.Minute)
			select {
			case <-doneChan:
				stopAndDrainTimer(timer)
			case <-timer.C:
				cli.Log.Warnf("Node handling is taking long for %s - continuing in background", node.XMLString())
			}
		case <-ctx.Done():
			cli.Log.Debugf("Closing handler queue loop")
			return
		}
	}
}

func (cli *Client) sendNodeAndGetData(node waBinary.Node) ([]byte, error) {
	t := cli.socketLock.RLock()
	sock := cli.socket
	cli.socketLock.RUnlock(t)
	if sock == nil {
		return nil, ErrNotConnected
	}

	payload, err := waBinary.Marshal(node)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal node: %w", err)
	}

	cli.sendLog.Debugf("%s", node.XMLString())
	return payload, sock.SendFrame(payload)
}

func (cli *Client) sendNode(node waBinary.Node) error {
	_, err := cli.sendNodeAndGetData(node)
	return err
}

func (cli *Client) dispatchEvent(evt interface{}) {
	t := cli.eventHandlersLock.RLock()
	defer func() {
		cli.eventHandlersLock.RUnlock(t)
		err := recover()
		if err != nil {
			cli.Log.Errorf("Event handler panicked while handling a %T: %v\n%s", evt, err, debug.Stack())
		}
	}()
	for _, handler := range cli.eventHandlers {
		handler.fn(evt)
	}
}

// ParseWebMessage parses a WebMessageInfo object into *events.Message to match what real-time messages have.
//
// The chat JID can be found in the Conversation data:
//
//	chatJID, err := types.ParseJID(conv.GetId())
//	for _, historyMsg := range conv.GetMessages() {
//		evt, err := cli.ParseWebMessage(chatJID, historyMsg.GetMessage())
//		yourNormalEventHandler(evt)
//	}
func (cli *Client) ParseWebMessage(chatJID types.JID, webMsg *waProto.WebMessageInfo) (*events.Message, error) {
	var err error
	if chatJID.IsEmpty() {
		chatJID, err = types.ParseJID(webMsg.GetKey().GetRemoteJid())
		if err != nil {
			return nil, fmt.Errorf("no chat JID provided and failed to parse remote JID: %w", err)
		}
	}
	info := types.MessageInfo{
		MessageSource: types.MessageSource{
			Chat:     chatJID,
			IsFromMe: webMsg.GetKey().GetFromMe(),
			IsGroup:  chatJID.Server == types.GroupServer,
		},
		ID:        webMsg.GetKey().GetId(),
		PushName:  webMsg.GetPushName(),
		Timestamp: time.Unix(int64(webMsg.GetMessageTimestamp()), 0),
	}
	if info.IsFromMe {
		info.Sender = cli.getOwnJID().ToNonAD()
		if info.Sender.IsEmpty() {
			return nil, ErrNotLoggedIn
		}
	} else if chatJID.Server == types.DefaultUserServer || chatJID.Server == types.NewsletterServer {
		info.Sender = chatJID
	} else if webMsg.GetParticipant() != "" {
		info.Sender, err = types.ParseJID(webMsg.GetParticipant())
	} else if webMsg.GetKey().GetParticipant() != "" {
		info.Sender, err = types.ParseJID(webMsg.GetKey().GetParticipant())
	} else {
		return nil, fmt.Errorf("couldn't find sender of message %s", info.ID)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to parse sender of message %s: %v", info.ID, err)
	}
	evt := &events.Message{
		RawMessage:   webMsg.GetMessage(),
		SourceWebMsg: webMsg,
		Info:         info,
	}
	evt.UnwrapRaw()
	return evt, nil
}

package proxy

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bedrock-tool/bedrocktool/ui/messages"
	"github.com/bedrock-tool/bedrocktool/utils"
	"github.com/gregwebs/go-recovery"
	"github.com/sandertv/gophertunnel/minecraft"
	"github.com/sandertv/gophertunnel/minecraft/protocol"
	"github.com/sandertv/gophertunnel/minecraft/protocol/login"
	"github.com/sandertv/gophertunnel/minecraft/protocol/packet"
	"github.com/sandertv/gophertunnel/minecraft/resource"
	"github.com/sirupsen/logrus"
)

type Context struct {
	Server           minecraft.IConn
	Client           minecraft.IConn
	expectDisconnect bool
	listener         *minecraft.Listener
	Player           Player
	ExtraDebug       bool
	PlayerMoveCB     []func()
	ListenAddress    string

	withClient bool
	addedPacks []resource.Pack

	dimensionData    *packet.DimensionData
	clientConnecting chan struct{}
	haveClientData   chan struct{}
	clientData       login.ClientData
	clientAddr       net.Addr
	spawned          bool
	disconnectReason string
	serverAddress    string
	serverName       string

	commands  map[string]ingameCommand
	handlers  []*Handler
	rpHandler *rpHandler
}

// New creates a new proxy context
func New(withClient bool) (*Context, error) {
	p := &Context{
		commands:         make(map[string]ingameCommand),
		withClient:       withClient,
		disconnectReason: "Connection Lost",
		ListenAddress:    "0.0.0.0:19132",
	}
	return p, nil
}

// AddCommand adds a command to the command handler
func (p *Context) AddCommand(exec func([]string) bool, cmd protocol.Command) {
	cmd.AliasesOffset = 0xffffffff
	p.commands[cmd.Name] = ingameCommand{exec, cmd}
}

// ClientWritePacket sends a packet to the client, nop if no client connected
func (p *Context) ClientWritePacket(pk packet.Packet) error {
	if p.Client == nil {
		return nil
	}
	return p.Client.WritePacket(pk)
}

// SendMessage sends a chat message to the client
func (p *Context) SendMessage(text string) {
	_ = p.ClientWritePacket(&packet.Text{
		TextType: packet.TextTypeSystem,
		Message:  "§8[§bBedrocktool§8]§r " + text,
	})
}

// SendPopup sends a toolbar popup to the client
func (p *Context) SendPopup(text string) {
	_ = p.ClientWritePacket(&packet.Text{
		TextType: packet.TextTypePopup,
		Message:  text,
	})
}

// AddHandler adds a handler to the proxy
func (p *Context) AddHandler(handler *Handler) {
	p.handlers = append(p.handlers, handler)
}

func (p *Context) commandHandlerPacketCB(pk packet.Packet, toServer bool, _ time.Time, _ bool) (packet.Packet, error) {
	switch _pk := pk.(type) {
	case *packet.CommandRequest:
		cmd := strings.Split(_pk.CommandLine, " ")
		name := cmd[0][1:]
		if h, ok := p.commands[name]; ok {
			pk = nil
			h.Exec(cmd[1:])
		}
	case *packet.AvailableCommands:
		cmds := make([]protocol.Command, 0, len(p.commands))
		for _, ic := range p.commands {
			cmds = append(cmds, ic.Cmd)
		}
		_pk.Commands = append(_pk.Commands, cmds...)
	}
	return pk, nil
}

func (p *Context) proxyLoop(ctx context.Context, toServer bool) (err error) {
	var c1, c2 minecraft.IConn
	if toServer {
		c1 = p.Client
		c2 = p.Server
	} else {
		c1 = p.Server
		c2 = p.Client
	}

	if false {
		defer func() {
			rec := recover()
			if rec != nil {
				if s, ok := rec.(string); ok {
					rec = errors.New(s)
				}
				err = rec.(error)
			}
		}()
	}

	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		pk, timeReceived, err := c1.ReadPacketWithTime()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				err = nil
			}
			return err
		}

		pkName := reflect.TypeOf(pk).String()
		for _, handler := range p.handlers {
			if handler.PacketCallback == nil {
				continue
			}
			pk, err = handler.PacketCallback(pk, toServer, timeReceived, false)
			if err != nil {
				return err
			}
			if pk == nil {
				logrus.Tracef("Dropped Packet: %s", pkName)
				break
			}
		}

		var transfer *packet.Transfer
		switch _pk := pk.(type) {
		case *packet.Transfer:
			transfer = _pk
			if p.Client != nil {
				host, port, err := net.SplitHostPort(p.Client.ClientData().ServerAddress)
				if err != nil {
					return err
				}
				// transfer to self
				_port, _ := strconv.Atoi(port)
				pk = &packet.Transfer{Address: host, Port: uint16(_port)}
			}
		}

		if pk != nil && c2 != nil {
			if err := c2.WritePacket(pk); err != nil {
				if disconnect, ok := errors.Unwrap(err).(minecraft.DisconnectError); ok {
					p.disconnectReason = disconnect.Error()
				}
				if errors.Is(err, net.ErrClosed) {
					err = nil
				}
				return err
			}
		}

		if transfer != nil {
			return errTransfer{transfer: transfer}
		}
	}
}

// Disconnect disconnects both the client and server
func (p *Context) Disconnect() {
	p.DisconnectClient()
	p.DisconnectServer()
}

// Disconnect disconnects the client
func (p *Context) DisconnectClient() {
	if p.Client == nil {
		return
	}
	_ = p.Client.Close()
}

// Disconnect disconnects from the server
func (p *Context) DisconnectServer() {
	if p.Server == nil {
		return
	}
	p.expectDisconnect = true
	_ = p.Server.Close()
}

func (p *Context) IsClient(addr net.Addr) bool {
	return p.clientAddr.String() == addr.String()
}

func (p *Context) packetFunc(header packet.Header, payload []byte, src, dst net.Addr) {
	defer func() {
		if err, ok := recover().(error); ok {
			recovery.ErrorHandler(err)
		}
	}()

	if header.PacketID == packet.IDRequestNetworkSettings {
		p.clientAddr = src
	}
	if header.PacketID == packet.IDSetLocalPlayerAsInitialised {
		p.spawned = true
	}

	for _, h := range p.handlers {
		if h.PacketRaw != nil {
			h.PacketRaw(header, payload, src, dst)
		}
	}

	if !p.spawned {
		pk, ok := DecodePacket(header, payload, p.Server.ShieldID())
		if !ok {
			return
		}

		switch pk := pk.(type) {
		case *packet.DimensionData:
			p.dimensionData = pk
		}

		var err error
		toServer := p.IsClient(src)
		for _, handler := range p.handlers {
			if handler.PacketCallback == nil {
				continue
			}
			pk, err = handler.PacketCallback(pk, toServer, time.Now(), !p.spawned)
			if err != nil {
				logrus.Error(err)
			}
			if pk == nil {
				break
			}
		}
	}
}

func (p *Context) onServerConnect() error {
	for _, handler := range p.handlers {
		if handler.OnServerConnect == nil {
			continue
		}
		disconnect, err := handler.OnServerConnect()
		if err != nil {
			return err
		}
		if disconnect {
			return errCancelConnect
		}
	}
	return nil
}

func (p *Context) doSession(ctx context.Context, cancel context.CancelCauseFunc) (err error) {
	defer func() {
		for _, handler := range p.handlers {
			if handler.OnSessionEnd != nil {
				handler.OnSessionEnd()
			}
		}
	}()

	isReplay := strings.HasPrefix(p.serverAddress, "PCAP!")
	if isReplay {
		p.serverName = path.Base(p.serverName)
	}

	for _, handler := range p.handlers {
		if handler.OnAddressAndName == nil {
			continue
		}
		err = handler.OnAddressAndName(p.serverAddress, p.serverName)
		if err != nil {
			return err
		}
	}

	if !isReplay {
		// ask for login before listening
		if !utils.Auth.LoggedIn() {
			messages.Router.Handle(&messages.Message{
				Source: "proxy",
				Target: "ui",
				Data: messages.RequestLogin{
					Wait: true,
				},
			})
		}
	}

	listenIP, _listenPort, _ := net.SplitHostPort(p.ListenAddress)
	listenPort, _ := strconv.Atoi(_listenPort)

	messages.Router.Handle(&messages.Message{
		Source: "proxy",
		Target: "ui",
		Data: messages.ConnectStateUpdate{
			State:      messages.ConnectStateBegin,
			ListenIP:   listenIP,
			ListenPort: listenPort,
		},
	})

	filterDownloadResourcePacks := func(id string) bool {
		ignore := false
		for _, handler := range p.handlers {
			if handler.FilterResourcePack != nil {
				ignore = handler.FilterResourcePack(id)
			}
		}
		return ignore
	}

	// setup Client and Server Connections
	wg := sync.WaitGroup{}
	if isReplay {
		filename := p.serverAddress[5:]
		server, err := CreateReplayConnector(ctx, filename, p.packetFunc, p.onResourcePacksInfo, p.onFinishedPack, filterDownloadResourcePacks)
		if err != nil {
			return err
		}
		p.Server = server
		err = server.ReadUntilLogin()
		if err != nil {
			return err
		}
	} else {
		p.rpHandler = newRpHandler(ctx, p.addedPacks, filterDownloadResourcePacks)
		p.rpHandler.OnResourcePacksInfoCB = p.onResourcePacksInfo
		p.rpHandler.OnFinishedPack = p.onFinishedPack

		if p.withClient {
			wg.Add(1)
			go func() {
				defer wg.Done()
				err = p.connectClient(ctx, p.serverAddress)
				if err != nil {
					cancel(err)
					return
				}
				for _, handler := range p.handlers {
					if handler.OnClientConnect == nil {
						continue
					}
					handler.OnClientConnect()
				}
			}()
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err = p.connectServer(ctx)
			if err != nil {
				cancel(err)
				return
			}
		}()
	}

	wg.Wait()
	if p.Server != nil {
		defer p.Server.Close()
	}
	if p.listener != nil {
		defer func() {
			if p.Client != nil {
				_ = p.listener.Disconnect(p.Client.(*minecraft.Conn), p.disconnectReason)
			}
			_ = p.listener.Close()
		}()
	}

	if ctx.Err() == nil {
		err = p.onServerConnect()
		if err != nil {
			cancel(err)
		}
	}

	if ctx.Err() != nil {
		err = context.Cause(ctx)
		if errors.Is(err, errCancelConnect) {
			err = nil
		}
		if err != nil {
			p.disconnectReason = err.Error()
		} else {
			p.disconnectReason = "Disconnect"
		}
		return err
	}

	{ // spawn
		gd := p.Server.GameData()
		for _, handler := range p.handlers {
			if handler.GameDataModifier == nil {
				continue
			}
			handler.GameDataModifier(&gd)
		}

		if p.Client != nil {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if p.dimensionData != nil {
					p.Client.WritePacket(p.dimensionData)
				}
				err := p.Client.StartGameContext(ctx, gd)
				if err != nil {
					cancel(err)
					return
				}
			}()
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			err := p.Server.DoSpawnContext(ctx)
			if err != nil {
				cancel(err)
				return
			}
		}()

		wg.Wait()
		err = context.Cause(ctx)
		if err != nil {
			p.disconnectReason = err.Error()
			return err
		}

		for _, handler := range p.handlers {
			if handler.OnConnect == nil {
				continue
			}
			if handler.OnConnect() {
				logrus.Info("Disconnecting")
				return nil
			}
		}
	}

	messages.Router.Handle(&messages.Message{
		Source: "proxy",
		Target: "ui",
		Data: messages.ConnectStateUpdate{
			State: messages.ConnectStateDone,
		},
	})

	{ // packet loop
		doProxy := func(client bool) {
			defer wg.Done()
			if err := p.proxyLoop(ctx, client); err != nil {
				if !errors.Is(err, context.Canceled) {
					cancel(err)
				}
				return
			}
			if p.Client != nil {
				p.Client.Close()
			}
			p.Server.Close()
		}

		// server to client
		wg.Add(1)
		go doProxy(false)

		// client to server
		if p.Client != nil {
			wg.Add(1)
			go doProxy(true)
		}

		wg.Wait()
		err = context.Cause(ctx)
		if err != nil {
			p.disconnectReason = err.Error()
		}
	}

	return err
}

type errTransfer struct {
	transfer *packet.Transfer
}

func (e errTransfer) Error() string {
	return fmt.Sprintf("transfer to %s:%d", e.transfer.Address, e.transfer.Port)
}

func (p *Context) connect(ctx context.Context) (err error) {
	p.spawned = false
	p.clientAddr = nil
	p.Client = nil
	p.clientConnecting = make(chan struct{})
	p.haveClientData = make(chan struct{})
	ctx2, cancel := context.WithCancelCause(ctx)
	err = p.doSession(ctx2, cancel)
	cancel(nil)

	if err, ok := err.(*errTransfer); ok {
		p.serverAddress = fmt.Sprintf("%s:%d", err.transfer.Address, err.transfer.Port)
		logrus.Infof("transferring to %s", p.serverAddress)
		return p.connect(ctx)
	}

	return err
}

func (p *Context) Run(ctx context.Context, connectString string) (err error) {
	var serverInput *messages.ServerInput
	if connectString != "" {
		serverInput, err = utils.ParseServer(context.Background(), connectString)
		if err != nil {
			return err
		}
	} else {
		resp := messages.Router.Handle(&messages.Message{
			Source: "proxy",
			Target: "ui",
			Data:   &messages.ServerInput{Request: true},
		})
		if err, ok := resp.Data.(messages.Error); ok {
			return err
		}
		serverInput = resp.Data.(*messages.ServerInput)
	}

	if serverInput.IsReplay {
		p.serverAddress = "PCAP!" + serverInput.Address
	} else {
		p.serverAddress = serverInput.Address + ":" + serverInput.Port
	}
	p.serverName = serverInput.Name

	if utils.Options.Debug || utils.Options.ExtraDebug {
		p.ExtraDebug = utils.Options.ExtraDebug
		p.AddHandler(NewDebugLogger(utils.Options.ExtraDebug))
	}
	if utils.Options.Capture {
		p.AddHandler(NewPacketCapturer())
	}
	p.AddHandler(&Handler{
		Name:           "Commands",
		PacketCallback: p.commandHandlerPacketCB,
	})
	p.AddHandler(&Handler{
		Name: "Player",
		PacketCallback: func(pk packet.Packet, toServer bool, timeReceived time.Time, preLogin bool) (packet.Packet, error) {
			haveMoved := p.Player.handlePackets(pk)
			if haveMoved {
				for _, cb := range p.PlayerMoveCB {
					cb()
				}
			}
			return pk, nil
		},
	})

	for _, handler := range p.handlers {
		if handler.ProxyReference != nil {
			handler.ProxyReference(p)
		}
	}

	defer func() {
		for _, handler := range p.handlers {
			if handler.OnProxyEnd != nil {
				handler.OnProxyEnd()
			}
		}
		messages.Router.Handle(&messages.Message{
			Source: "proxy",
			Target: "ui",
			Data:   messages.UIStateFinished,
		})
	}()

	// load forced packs
	if _, err := os.Stat("forcedpacks"); err == nil {
		if err = filepath.WalkDir("forcedpacks/", func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				return nil
			}
			ext := filepath.Ext(path)
			switch ext {
			case ".mcpack", ".zip":
				pack, err := resource.ReadPath(path)
				if err != nil {
					return err
				}
				p.addedPacks = append(p.addedPacks, pack)
				logrus.Infof("Added %s to the forced packs", pack.Name())
			default:
				logrus.Warnf("Unrecognized file %s in forcedpacks", path)
			}
			return nil
		}); err != nil {
			return err
		}
	}

	return p.connect(ctx)
}

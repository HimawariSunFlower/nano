// Copyright (c) nano Authors. All Rights Reserved.
//
// Permission is hereby granted, free of charge, to any person obtaining a copy
// of this software and associated documentation files (the "Software"), to deal
// in the Software without restriction, including without limitation the rights
// to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
// copies of the Software, and to permit persons to whom the Software is
// furnished to do so, subject to the following conditions:
//
// The above copyright notice and this permission notice shall be included in all
// copies or substantial portions of the Software.
//
// THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
// IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
// FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
// AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
// LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
// OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
// SOFTWARE.

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/lonng/nano/cluster/clusterpb"
	"github.com/lonng/nano/component"
	"github.com/lonng/nano/internal/codec"
	"github.com/lonng/nano/internal/env"
	"github.com/lonng/nano/internal/log"
	"github.com/lonng/nano/internal/message"
	"github.com/lonng/nano/internal/packet"
	"github.com/lonng/nano/metrics"
	"github.com/lonng/nano/pipeline"
	"github.com/lonng/nano/scheduler"
	"github.com/lonng/nano/session"
)

var (
	// cached serialized data
	hrd []byte // handshake response data
	hbd []byte // heartbeat packet data
)

type rpcHandler func(session *session.Session, msg *message.Message, noCopy bool)

func cache() {

	sysMap := map[string]interface{}{
		"heartbeat": env.Heartbeat.Seconds(),
		"dict":      env.RouteDict,
		//"protos":
	}

	protoMsgJson, err := ioutil.ReadFile("./configs/proto_pomelo.json")
	if err == nil {
		log.Println("Register proto json ./configs/proto_msg.json")
		var msgJson map[string]interface{}
		json.Unmarshal(protoMsgJson, &msgJson)
		sysMap["protos"] = map[string]interface{}{
			"server": msgJson,
		}
	}

	data, err := json.Marshal(map[string]interface{}{
		"code": 200,
		"sys":  sysMap,
	})
	if err != nil {
		panic(err)
	}

	hrd, err = codec.Encode(packet.Handshake, data)
	if err != nil {
		panic(err)
	}

	hbd, err = codec.Encode(packet.Heartbeat, nil)
	if err != nil {
		panic(err)
	}
}

type LocalHandler struct {
	localServices        map[string]*component.Service // all registered service
	localHandlers        map[string]*component.Handler // all handler method
	localHandlersArgName map[string]*component.Handler // all handler method 参数名称映射

	mu             sync.RWMutex
	remoteServices map[string][]*clusterpb.MemberInfo

	pipeline    pipeline.Pipeline
	currentNode *Node
	rateLimiter *env.RateLimiter
}

func NewHandler(currentNode *Node, pipeline pipeline.Pipeline) *LocalHandler {
	h := &LocalHandler{
		localServices:        make(map[string]*component.Service),
		localHandlers:        make(map[string]*component.Handler),
		localHandlersArgName: make(map[string]*component.Handler),
		remoteServices:       map[string][]*clusterpb.MemberInfo{},
		pipeline:             pipeline,
		currentNode:          currentNode,
		rateLimiter:          env.NewRateLimiter(currentNode.RateLimit),
	}

	return h
}

func (h *LocalHandler) register(comp component.Component, opts []component.Option) error {
	s := component.NewService(comp, opts)

	if _, ok := h.localServices[s.Name]; ok {
		return fmt.Errorf("handler: service already defined: %s", s.Name)
	}

	if err := s.ExtractHandler(); err != nil {
		return err
	}

	// register all localHandlers
	h.localServices[s.Name] = s
	doubleNames := make([]string, 0)
	for name, handler := range s.Handlers {
		n := fmt.Sprintf("%s.%s", s.Name, name)
		if env.ProtoRoute {
			//以控制器第二个参数 结构体名称,为路由
			argTypeName := handler.Type.Elem().Name()
			if _, exist := h.localHandlersArgName[argTypeName]; !exist {
				//log.Println(fmt.Sprintf("Proto Register local handler %s-%s", s.Name, argTypeName))
				h.localHandlersArgName[argTypeName] = handler
			} else {
				doubleNames = append(doubleNames, n+"("+argTypeName+")")
			}
		} else {
			log.Println("Register local handler", n)
			h.localHandlers[n] = handler
		}
	}
	if len(doubleNames) > 0 {
		log.Fatalf("重复消息协议 %v", doubleNames)
	}
	return nil
}

func (h *LocalHandler) initRemoteService(members []*clusterpb.MemberInfo) {
	for _, m := range members {
		h.addRemoteService(m)
	}
}

func (h *LocalHandler) addRemoteService(member *clusterpb.MemberInfo) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for _, s := range member.Services {
		log.Println("Register remote service", s)
		h.remoteServices[s] = append(h.remoteServices[s], member)
	}
}

func (h *LocalHandler) delMember(addr string) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for name, members := range h.remoteServices {
		for i, maddr := range members {
			if addr == maddr.ServiceAddr {
				if i == len(members)-1 {
					members = members[:i]
				} else {
					members = append(members[:i], members[i+1:]...)
				}
			}
		}
		if len(members) == 0 {
			delete(h.remoteServices, name)
		} else {
			h.remoteServices[name] = members
		}
	}
}

func (h *LocalHandler) LocalService() []string {
	var result []string
	for service := range h.localServices {
		result = append(result, service)
	}
	sort.Strings(result)
	return result
}

func (h *LocalHandler) RemoteService() []string {
	h.mu.RLock()
	defer h.mu.RUnlock()

	var result []string
	for service := range h.remoteServices {
		result = append(result, service)
	}
	sort.Strings(result)
	return result
}

func (h *LocalHandler) handle(conn net.Conn) {
	// create a client agent and startup write gorontine
	agent := newAgent(conn, h.pipeline, h.remoteProcess)
	h.currentNode.storeSession(agent.session)

	// startup write goroutine
	go agent.write()

	if env.Debug {
		log.Println(fmt.Sprintf("New session established: %s", agent.String()))
	}

	// guarantee agent related resource be destroyed
	defer func() {
		request := &clusterpb.SessionClosedRequest{
			SessionId: agent.session.ID(),
		}

		members := h.currentNode.cluster.remoteAddrs()
		for _, remote := range members {
			log.Println("Notify remote server success", remote)
			pool, err := h.currentNode.rpcClient.getConnPool(remote)
			if err != nil {
				log.Println("Cannot retrieve connection pool for address", remote, err)
				continue
			}
			client := clusterpb.NewMemberClient(pool.Get())
			_, err = client.SessionClosed(context.Background(), request)
			if err != nil {
				log.Println("Cannot closed session in remote address", remote, err)
				continue
			}
			if env.Debug {
				log.Println("Notify remote server success", remote)
			}
		}
		h.currentNode.removeSession(agent.session)
		agent.Close()
		if env.Debug {
			log.Println(fmt.Sprintf("Session read goroutine exit, SessionID=%d, UID=%d", agent.session.ID(), agent.session.UID()))
		}
	}()

	// read loop
	buf := make([]byte, 2048)
	for {
		n, err := conn.Read(buf)
		if err != nil {
			log.Println(fmt.Sprintf("Read message error: %s, session will be closed immediately", err.Error()))
			return
		}

		// TODO(warning): decoder use slice for performance, packet data should be copy before next Decode
		packets, err := agent.decoder.Decode(buf[:n])
		if err != nil {
			log.Println(err.Error())
			return
		}

		if len(packets) < 1 {
			continue
		}

		now := time.Now()
		// process all packet
		for i := range packets {
			if h.rateLimiter != nil {
				if h.rateLimiter.ShouldRateLimit(now) {
					metrics.ReportExceededRateLimiting(h.currentNode.MetricsReporters)
					log.Println("Receive packets exceed rate limit!")
					return
				}
			}
			if err := h.processPacket(agent, packets[i]); err != nil {
				log.Println(err.Error())
				return
			}
		}
	}
}

func (h *LocalHandler) processPacket(agent *agent, p *packet.Packet) error {
	switch p.Type {
	case packet.Handshake:
		if _, err := agent.conn.Write(hrd); err != nil {
			return err
		}

		agent.setStatus(statusHandshake)
		if env.Debug {
			log.Println(fmt.Sprintf("Session handshake Id=%d, Remote=%s", agent.session.ID(), agent.conn.RemoteAddr()))
		}

	case packet.HandshakeAck:
		agent.setStatus(statusWorking)
		if env.Debug {
			log.Println(fmt.Sprintf("Receive handshake ACK Id=%d, Remote=%s", agent.session.ID(), agent.conn.RemoteAddr()))
		}

	case packet.Data:
		if agent.status() < statusWorking {
			return fmt.Errorf("receive data on socket which not yet ACK, session will be closed immediately, remote=%s",
				agent.conn.RemoteAddr().String())
		}

		msg, err := message.Decode(p.Data)
		if err != nil {
			return err
		}
		h.processMessage(agent, msg)

	case packet.Heartbeat:
		// expected
	}

	agent.lastAt = time.Now().Unix()
	return nil
}

func (h *LocalHandler) findMembers(service string) []*clusterpb.MemberInfo {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return h.remoteServices[service]
}

func (h *LocalHandler) remoteProcess(session *session.Session, msg *message.Message, noCopy bool) {
	index := strings.LastIndex(msg.Route, ".")
	if index < 0 {
		log.Println(fmt.Sprintf("nano/handler: invalid route %s", msg.Route))
		return
	}

	service := msg.Route[:index]
	members := h.findMembers(service)
	if len(members) == 0 {
		log.Println(fmt.Sprintf("nano/handler: %s not found(forgot registered?)", msg.Route))
		return
	}

	// Select a remote service address
	// 1. Use the service address directly if the router contains binding item
	// 2. Select a remote service address randomly and bind to router
	var remoteAddr string
	if addr, found := session.Router().Find(service); found {
		remoteAddr = addr
	} else {
		remoteAddr = members[rand.Intn(len(members))].ServiceAddr
		session.Router().Bind(service, remoteAddr)
	}
	pool, err := h.currentNode.rpcClient.getConnPool(remoteAddr)
	if err != nil {
		log.Println(err)
		return
	}
	var data = msg.Data
	if !noCopy && len(msg.Data) > 0 {
		data = make([]byte, len(msg.Data))
		copy(data, msg.Data)
	}

	// Retrieve gate address and session id
	gateAddr := h.currentNode.ServiceAddr
	sessionId := session.ID()
	switch v := session.NetworkEntity().(type) {
	case *acceptor:
		gateAddr = v.gateAddr
		sessionId = v.sid
	}

	client := clusterpb.NewMemberClient(pool.Get())
	switch msg.Type {
	case message.Request:
		request := &clusterpb.RequestMessage{
			GateAddr:  gateAddr,
			SessionId: sessionId,
			Id:        msg.ID,
			Route:     msg.Route,
			Data:      data,
		}
		_, err = client.HandleRequest(context.Background(), request)
	case message.Notify:
		request := &clusterpb.NotifyMessage{
			GateAddr:  gateAddr,
			SessionId: sessionId,
			Route:     msg.Route,
			Data:      data,
		}
		_, err = client.HandleNotify(context.Background(), request)
	}
	if err != nil {
		log.Println(fmt.Sprintf("Process remote message (%d:%s) error: %+v", msg.ID, msg.Route, err))
	}
}

func (h *LocalHandler) processMessage(agent *agent, msg *message.Message) {
	metrics.CountMessage()

	var lastMid uint64
	switch msg.Type {
	case message.Request:
		lastMid = msg.ID
	case message.Notify:
		lastMid = 0
	default:
		log.Println("Invalid message type: " + msg.Type.String())
		return
	}
	if env.ProtoRoute {
		handler, found := h.localHandlersArgName[msg.Route]
		if !found {
			h.remoteProcess(agent.session, msg, false)
		} else {
			h.localProcess(handler, lastMid, agent.session, msg)
		}
	} else {
		handler, found := h.localHandlers[msg.Route]
		if !found {
			h.remoteProcess(agent.session, msg, false)
		} else {
			h.localProcess(handler, lastMid, agent.session, msg)
		}
	}
}

func (h *LocalHandler) handleWS(conn *websocket.Conn) {
	c, err := newWSConn(conn)
	if err != nil {
		log.Println(err)
		return
	}
	go h.handle(c)
}

func (h *LocalHandler) localProcess(handler *component.Handler, lastMid uint64, session *session.Session, msg *message.Message) {
	org_start := time.Now().UnixNano()
	if pipe := h.pipeline; pipe != nil {
		err := pipe.Inbound().Process(session, msg)
		if err != nil {
			log.Println("Pipeline process failed: " + err.Error())
			return
		}
	}

	var payload = msg.Data
	var data interface{}
	if handler.IsRawArg {
		data = payload
	} else {
		data = reflect.New(handler.Type.Elem()).Interface()
		err := env.Serializer.Unmarshal(payload, data)
		if err != nil {
			log.Println(fmt.Sprintf("Deserialize to %T failed: %+v (%v)", data, err, payload))
			return
		}
	}

	if env.Debug {
		log.Println(fmt.Sprintf("UID=%d, Message={%s}, Data=%+v", session.UID(), msg.String(), data))
	}

	session.Set("route", msg.Route)
	args := []reflect.Value{handler.Receiver, reflect.ValueOf(session), reflect.ValueOf(data)}

	task := func() {
		os := org_start
		route := msg.Route

		metrics.ReportMessageProcessDelay(os, h.currentNode.MetricsReporters, route)
		switch v := session.NetworkEntity().(type) {
		case *agent:
			v.lastMid = lastMid
		case *acceptor:
			v.lastMid = lastMid
		}

		start := time.Now().UnixNano()
		if env.Debug {
			log.Println(fmt.Sprintf("--%s time start ----", handler.Method.Func.String()))
		}
		//前置处理
		if h.currentNode.FuncBefore != nil && !h.currentNode.FuncBefore(session, data) {
			if env.Debug {
				log.Println(fmt.Sprintf("--%s FuncBefore exit ", handler.Method.Func.String()))
			}
			return
		}

		result := handler.Method.Func.Call(args)
		metrics.ReportTiming(os, h.currentNode.MetricsReporters, route)
		if len(result) > 0 {
			if err := result[0].Interface(); err != nil {
				log.Println(fmt.Sprintf("Service %s error: %+v", msg.Route, err))
			}
		}
		//后置处理
		if h.currentNode.FuncAfter != nil {
			h.currentNode.FuncAfter(session, data)
		}

		if env.Debug {
			end := time.Now().Unix()
			log.Println(fmt.Sprintf("--%s time end:%d", handler.Method.Func.String(), end-start))
		}
	}
	var serCase *component.Service

	if handler.ParentService != nil {
		serCase = handler.ParentService
	} else {
		index := strings.LastIndex(msg.Route, ".")
		if index < 0 {
			log.Println(fmt.Sprintf("nano/handler: invalid route %s", msg.Route))
			return
		}
		// A message can be dispatch to global thread or a user customized thread
		service := msg.Route[:index]
		serCase = h.localServices[service]
	}

	if serCase.SchedName != "" {
		sched := session.Value(serCase.SchedName)
		if sched == nil {
			log.Println(fmt.Sprintf("nanl/handler: cannot found `schedular.LocalScheduler` by %s", serCase.SchedName))
			return
		}

		local, ok := sched.(scheduler.LocalScheduler)
		if !ok {
			log.Println(fmt.Sprintf("nanl/handler: Type %T does not implement the `schedular.LocalScheduler` interface",
				sched))
			return
		}
		local.Schedule(task)
	} else {
		scheduler.PushTask(task)
	}
}

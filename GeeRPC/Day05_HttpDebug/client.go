package geerpc

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"geerpc/codec"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type Call struct {
	Seq           uint64
	ServiceMethod string // format "<service>.<method>"
	Args          interface{}
	Reply         interface{}
	Error         error
	Done          chan *Call // 当调用结束时，会调用 call.done() 通知调用方。
}

func (call *Call) done() {
	call.Done <- call
}

/*
Client
cc 是消息的编解码器，和服务端类似，用来序列化将要发送出去的请求，以及反序列化接收到的响应。
sending 是一个互斥锁，和服务端类似，为了保证请求的有序发送，即防止出现多个请求报文混淆。
header 是每个请求的消息头，header 只有在请求发送时才需要，而请求发送是互斥的，因此每个客户端只需要一个，声明在 Client 结构体中可以复用。
seq 用于给发送的请求编号，每个请求拥有唯一编号。
pending 存储未处理完的请求，键是编号，值是 Call 实例。
closing 和 shutdown 任意一个值置为 true，则表示 Client 处于不可用的状态，但有些许的差别，

	closing 是用户主动关闭的，即调用 Close 方法，
	而 shutdown 置为 true 一般是有错误发生。
*/
type Client struct {
	cc       codec.Codec
	mu       sync.Mutex // 保证整个 client 的发送成功
	sending  sync.Mutex // 保证 client 的多个请求报文不混淆
	header   codec.Header
	seq      uint64
	pending  map[uint64]*Call // 存储未处理完的请求，键是编号，值是 Call 实例。
	closing  bool             // 一般是用户主动调用 Close 方法进行关闭
	shutdown bool             // 一般是有错误发生 server 要求关闭
	option   *Option
}

/*
创建 Client 实例对象
*/
func NewClient(conn net.Conn, option *Option) (*Client, error) {
	codecFunc := codec.NewCodecFuncMap[option.CodecType]
	if codecFunc == nil {
		err := fmt.Errorf("invalid codec type %s", option.CodecType)
		log.Println("[NewClient] rpc client: codec error:", err)
		return nil, err
	}
	// 发送
	if err := json.NewEncoder(conn).Encode(option); err != nil {
		log.Println("[NewClient] rpc client: encode error:", err)
		_ = conn.Close()
		return nil, err
	}
	return newClientCodec(codecFunc(conn), option), nil
}

// newClientCodec 创建一个子协程调用 receive() 接收响应。
func newClientCodec(cc codec.Codec, option *Option) *Client {
	client := &Client{
		cc:      cc,
		seq:     0,
		pending: make(map[uint64]*Call),
		option:  option,
	}
	go client.receive()
	return client
}

func parseOptions(options ...*Option) (*Option, error) {
	// if params is nil, 或者放了一个 nil 参数
	if len(options) == 0 || options[0] == nil {
		return DefaultOption, nil
	}
	if len(options) != 1 {
		return nil, errors.New("[parseOptions] number of options is more than 1")
	}
	opt := options[0]
	// magic Number 必须一致
	opt.MagicNumber = DefaultOption.MagicNumber
	if opt.CodecType == "" {
		opt.CodecType = DefaultOption.CodecType
	}
	return opt, nil
}

/*
Client 的超时处理
*/
type clientResult struct {
	client *Client
	err    error
}

type newClientFunc func(conn net.Conn, opt *Option) (client *Client, err error)

func dialTimeout(clientFunc newClientFunc, network, address string, opts ...*Option) (client *Client, err error) {
	options, err := parseOptions(opts...)
	if err != nil {
		return nil, err
	}
	// DialTimeout 采用了超时
	conn, err := net.DialTimeout(network, address, options.ConnectTimeout)
	if err != nil {
		return nil, err
	}

	defer func() {
		if err != nil {
			_ = conn.Close()
		}
	}()

	ch := make(chan clientResult)
	// 使用子协程执行 NewClient，执行完成后则通过信道 ch 发送结果
	go func() {
		client, err := clientFunc(conn, options)
		ch <- clientResult{client: client, err: err}
	}()

	if options.ConnectTimeout == 0 {
		result := <-ch
		return result.client, result.err
	}

	// 如果 time.After() 信道先接收到消息，则说明 NewClient 执行超时，返回错误。
	select {
	case <-time.After(options.ConnectTimeout):
		return nil, fmt.Errorf("rpc client: connect timeout: expect within %s", options.ConnectTimeout)
	case result := <-ch:
		return result.client, result.err
	}
}

func Dial(network, address string, opts ...*Option) (*Client, error) {
	return dialTimeout(NewClient, network, address, opts...)
}

/*
	Client 的发送功能
*/
// Call 函数
func (client *Client) Call(ctx context.Context, serviceMethod string, args, reply interface{}) error {
	call := client.Go(serviceMethod, args, reply, make(chan *Call, 1))

	select {
	case <-ctx.Done():
		client.removeCall(call.Seq)
		return errors.New("rpc client: call failed: " + ctx.Err().Error())
	case call = <-call.Done:
		return call.Error
	}
}

// Go 有一个异步接口，返回 call 实例
func (client *Client) Go(serviceMethod string, args, reply interface{}, done chan *Call) *Call {
	if done == nil {
		done = make(chan *Call, 10)
	} else if cap(done) == 0 {
		log.Panic("[Go] rpc client: done channel is unbuffered")
	}
	call := &Call{
		Seq:           0,
		ServiceMethod: serviceMethod,
		Args:          args,
		Reply:         reply,
		Done:          done,
	}
	client.send(call)
	return call
}

func (client *Client) send(call *Call) {
	// 确保 client 将要发送完整的请求
	client.sending.Lock()
	defer client.sending.Unlock()

	// register
	seq, err := client.registerCall(call)
	if err != nil {
		call.Error = err
		call.done()
		return
	}

	// 准备 request header
	client.header.ServiceMethod = call.ServiceMethod
	client.header.Seq = seq
	client.header.Error = ""

	// 编码发送
	if err := client.cc.Write(&client.header, call.Args); err != nil {
		call := client.removeCall(seq)
		// call 可能不存在，可能是请求没有发送完整，或者因为其他原因被取消，但是服务端仍旧处理了。
		if call != nil {
			call.Error = err
			call.done()
		}
	}
}

/*
receive Client 的接收功能
*/
func (client *Client) receive() {
	var err error
	for err == nil {
		var header codec.Header
		if err = client.cc.ReadHeader(&header); err != nil {
			break
		}
		call := client.removeCall(header.Seq)
		switch {
		case call == nil:
			// 通常是请求没有发送完整，或者被取消了
			err = client.cc.ReadBody(nil)
		// 服务端处理出错（因此 Error 不为空
		case header.Error != "":
			call.Error = errors.New(header.Error)
			err = client.cc.ReadBody(nil)
			call.done()
		default:
			err = client.cc.ReadBody(call.Reply)
			if err != nil {
				log.Printf("[receive] readBody failed! err: %v", err)
				call.Error = errors.New("reading body" + err.Error())
			}
			call.done()
		}
	}
	// 发生错误，因此关闭所有的发送
	client.terminateCalls(err)
}

/*
registerCall：将参数 call 添加到 client.pending 中，并更新 client.seq。
removeCall：根据 seq，从 client.pending 中移除对应的 call，并返回。
terminateCalls：服务端或客户端发生错误时调用，将 shutdown 设置为 true，且将错误信息通知所有 pending 状态的 call。
*/
func (client *Client) registerCall(call *Call) (uint64, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	if client.closing || client.shutdown {
		return 0, ErrShutdown
	}
	call.Seq = client.seq
	client.pending[call.Seq] = call
	client.seq++
	return call.Seq, nil
}

func (client *Client) removeCall(seq uint64) *Call {
	client.mu.Lock()
	defer client.mu.Unlock()
	call := client.pending[seq]
	delete(client.pending, seq)
	return call
}

func (client *Client) terminateCalls(err error) {
	// 将错误信息通知所有 pending 状态的 call。
	client.sending.Lock()
	defer client.sending.Unlock()

	client.mu.Lock()
	defer client.mu.Unlock()
	client.shutdown = true
	for _, call := range client.pending {
		call.Error = err
		call.done()
	}
}

/*
	关闭功能
*/

var ErrShutdown = errors.New("connection is shutdown")

// Close the connection
func (client *Client) Close() error {
	client.mu.Lock()
	defer client.mu.Unlock()

	if client.closing {
		return ErrShutdown
	}
	client.closing = true
	return client.cc.Close()
}

// IsAvailable 用于判断 client 是否是正常工作的
func (client *Client) IsAvailable() bool {
	client.mu.Lock()
	defer client.mu.Unlock()
	return !client.shutdown && !client.closing
}

/*
客户端支持 HTTP  发起 CONNECT 请求，检查返回状态码即可成功建立连接。
*/
func NewHTTPClient(conn net.Conn, opt *Option) (*Client, error) {
	_, _ = io.WriteString(conn, fmt.Sprintf("CONNECT %s HTTP/1.0 \n\n", defaultRPCPath))

	// Require successful HTTP response
	// before switching to RPC protocol.
	response, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: "CONNECT"})
	if err == nil && response.Status == connected {
		return NewClient(conn, opt)
	}
	if err == nil {
		err = errors.New("unexpected HTTP response: " + response.Status)
	}
	return nil, err
}

func DialHTTP(network, address string, opts ...*Option) (*Client, error) {
	return dialTimeout(NewHTTPClient, network, address, opts...)
}

// XDial calls different func to connect to a RPC server
// according to Lexer param rpcAddr
// rpcAddr is a general format (protocol@addr) to represent a rpc server
// eg, http@10.0.0.1:7001, tcp@10.0.0.1:9999, unix@/tmp/geerpc.sock
func XDial(rpcAddr string, options ...*Option) (*Client, error) {
	parts := strings.Split(rpcAddr, "@")
	if len(parts) != 2 {
		return nil, fmt.Errorf("[XDial] rpc client err: wrong format '%s', expect protocol@addr", rpcAddr)
	}
	protocol, addr := parts[0], parts[1]
	switch protocol {
	case "http":
		return DialHTTP("tcp", addr, options...)
	default:
		// tcp unix or other transport protocol
		return Dial(protocol, addr, options...)
	}
}

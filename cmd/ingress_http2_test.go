package cmd

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hashicorp/yamux"
	"golang.org/x/net/http2"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	pb "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type ingressRPCFixture struct {
	pb.UnimplementedTestServiceServer
	canceled chan struct{}
}

func (s *ingressRPCFixture) UnaryCall(ctx context.Context, r *pb.SimpleRequest) (*pb.SimpleResponse, error) {
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get("test-bin")) > 0 {
		_ = grpc.SendHeader(ctx, metadata.Pairs("echo-bin", md.Get("test-bin")[0]))
	}
	grpc.SetTrailer(ctx, metadata.Pairs("test-trailer", "delivered"))
	return &pb.SimpleResponse{Payload: r.Payload}, nil
}
func (s *ingressRPCFixture) StreamingOutputCall(r *pb.StreamingOutputCallRequest, stream pb.TestService_StreamingOutputCallServer) error {
	for _, p := range r.ResponseParameters {
		if err := stream.Send(&pb.StreamingOutputCallResponse{Payload: &pb.Payload{Body: bytes.Repeat([]byte("s"), int(p.Size))}}); err != nil {
			return err
		}
	}
	stream.SetTrailer(metadata.Pairs("stream-trailer", "done"))
	return nil
}
func (s *ingressRPCFixture) StreamingInputCall(stream pb.TestService_StreamingInputCallServer) error {
	var n int32
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			return stream.SendAndClose(&pb.StreamingInputCallResponse{AggregatedPayloadSize: n})
		}
		if err != nil {
			return err
		}
		n += int32(len(r.GetPayload().GetBody()))
	}
}
func (s *ingressRPCFixture) FullDuplexCall(stream pb.TestService_FullDuplexCallServer) error {
	for {
		r, err := stream.Recv()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if string(r.GetPayload().GetBody()) == "wait" {
			<-stream.Context().Done()
			s.canceled <- struct{}{}
			return status.FromContextError(stream.Context().Err()).Err()
		}
		if err := stream.Send(&pb.StreamingOutputCallResponse{Payload: r.Payload}); err != nil {
			return err
		}
	}
}

func ingressGRPCClient(t *testing.T) (pb.TestServiceClient, *ingressRPCFixture) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	origin := grpc.NewServer(grpc.MaxRecvMsgSize(8<<20), grpc.MaxSendMsgSize(8<<20))
	fixture := &ingressRPCFixture{canceled: make(chan struct{}, 4)}
	pb.RegisterTestServiceServer(origin, fixture)
	go origin.Serve(ln)
	t.Cleanup(origin.Stop)
	a, b := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	server, err := yamux.Server(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Close() })
	client, err := yamux.Client(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	state := &clientState{TunnelMap: map[string]string{"rpc": ln.Addr().String()}}
	go func() {
		for {
			stream, err := client.Accept()
			if err != nil {
				return
			}
			go handleMuxStream(ctx, stream, state)
		}
	}()
	info := &serverClientInfo{muxSession: server, tunnelMap: map[string]string{"rpc": ln.Addr().String()}}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 2 {
			http.Error(w, "HTTP/2 required", 400)
			return
		}
		serveHTTP2Tunnel(w, r, info, "rpc", "", nil)
	}))
	ts.EnableHTTP2 = true
	if err := configureIngressHTTP2(ts.Config); err != nil {
		t.Fatal(err)
	}
	ts.StartTLS()
	t.Cleanup(ts.Close)
	tlsConfig := ts.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	tlsConfig.NextProtos = []string{"h2"}
	conn, err := grpc.NewClient(strings.TrimPrefix(ts.URL, "https://"), grpc.WithTransportCredentials(credentials.NewTLS(tlsConfig)), grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(8<<20), grpc.MaxCallSendMsgSize(8<<20)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return pb.NewTestServiceClient(conn), fixture
}

func TestHTTP2IngressGRPCMethodsMetadataTrailersLargeMessages(t *testing.T) {
	c, _ := ingressGRPCClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ctx = metadata.NewOutgoingContext(ctx, metadata.Pairs("test-bin", "\x00\xffbinary"))
	var header, trailer metadata.MD
	large := bytes.Repeat([]byte("message"), 750000)
	r, err := c.UnaryCall(ctx, &pb.SimpleRequest{Payload: &pb.Payload{Body: large}}, grpc.Header(&header), grpc.Trailer(&trailer))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(r.GetPayload().GetBody(), large) || header.Get("echo-bin")[0] != "\x00\xffbinary" || trailer.Get("test-trailer")[0] != "delivered" {
		t.Fatal("unary payload/metadata/trailers changed")
	}
	output, err := c.StreamingOutputCall(ctx, &pb.StreamingOutputCallRequest{ResponseParameters: []*pb.ResponseParameters{{Size: 3}, {Size: 4}, {Size: 5}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{3, 4, 5} {
		r, err := output.Recv()
		if err != nil || len(r.GetPayload().GetBody()) != n {
			t.Fatalf("server stream: %v", err)
		}
	}
	if _, err = output.Recv(); err != io.EOF || output.Trailer().Get("stream-trailer")[0] != "done" {
		t.Fatal("server stream trailers lost")
	}
	input, err := c.StreamingInputCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if err = input.Send(&pb.StreamingInputCallRequest{Payload: &pb.Payload{Body: []byte("abcd")}}); err != nil {
			t.Fatal(err)
		}
	}
	aggregate, err := input.CloseAndRecv()
	if err != nil || aggregate.GetAggregatedPayloadSize() != 12 {
		t.Fatalf("client stream: %v", err)
	}
	bidi, err := c.FullDuplexCall(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 4; i++ {
		body := []byte{byte(i)}
		if err = bidi.Send(&pb.StreamingOutputCallRequest{Payload: &pb.Payload{Body: body}}); err != nil {
			t.Fatal(err)
		}
		r, err := bidi.Recv()
		if err != nil || !bytes.Equal(r.GetPayload().GetBody(), body) {
			t.Fatalf("bidi: %v", err)
		}
	}
	_ = bidi.CloseSend()
	if _, err = bidi.Recv(); err != io.EOF {
		t.Fatal(err)
	}
}

func TestHTTP2IngressGRPCCancellationAndDeadline(t *testing.T) {
	c, fixture := ingressGRPCClient(t)
	for _, deadline := range []bool{false, true} {
		var ctx context.Context
		var cancel context.CancelFunc
		if deadline {
			ctx, cancel = context.WithTimeout(context.Background(), 150*time.Millisecond)
		} else {
			ctx, cancel = context.WithCancel(context.Background())
		}
		stream, err := c.FullDuplexCall(ctx)
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if err = stream.Send(&pb.StreamingOutputCallRequest{Payload: &pb.Payload{Body: []byte("wait")}}); err != nil {
			cancel()
			t.Fatal(err)
		}
		if !deadline {
			time.Sleep(50 * time.Millisecond)
			cancel()
		}
		_, err = stream.Recv()
		cancel()
		code := codes.Canceled
		if deadline {
			code = codes.DeadlineExceeded
		}
		if status.Code(err) != code {
			t.Fatalf("status %v, want %v", err, code)
		}
		select {
		case <-fixture.canceled:
		case <-time.After(2 * time.Second):
			t.Fatal("origin RPC context was not canceled")
		}
	}
}

func TestHTTP2IngressAuthorizationAndUnavailable(t *testing.T) {
	client := &serverClientInfo{}
	r := httptest.NewRequest("POST", "https://rpc.example/service/call", nil)
	r.ProtoMajor = 2
	r.Header.Set("Content-Type", "application/grpc")
	w := httptest.NewRecorder()
	serveHTTP2Tunnel(w, r, client, "rpc", "user:pass", nil)
	if w.Code != 401 {
		t.Fatal("missing BasicAuth was accepted")
	}
	r.SetBasicAuth("user", "pass")
	w = httptest.NewRecorder()
	serveHTTP2Tunnel(w, r, client, "rpc", "user:pass", nil)
	if w.Result().Trailer.Get("Grpc-Status") != "14" {
		t.Fatalf("missing gRPC unavailable trailer: %+v", w.Result())
	}
}

func TestHTTP2IngressToHTTP1Backend(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 || r.Header.Get("X-Real-IP") == "" || r.Header.Get("X-Injected") != "yes" {
			http.Error(w, "forwarding metadata lost", 500)
			return
		}
		w.Header().Set("Trailer", "Echo-Trailer")
		_, _ = io.Copy(w, r.Body)
		w.Header().Set("Echo-Trailer", "delivered")
	}))
	defer origin.Close()
	a, b := net.Pipe()
	cfg := yamux.DefaultConfig()
	cfg.LogOutput = io.Discard
	session, err := yamux.Server(a, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	peer, err := yamux.Client(b, cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	state := &clientState{TunnelMap: map[string]string{"web": strings.TrimPrefix(origin.URL, "http://")}}
	go func() {
		for {
			c, err := peer.Accept()
			if err != nil {
				return
			}
			go handleMuxStream(ctx, c, state)
		}
	}()
	info := &serverClientInfo{muxSession: session, tunnelMap: state.TunnelMap}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveHTTP2Tunnel(w, r, info, "web", "u:p", map[string]string{"X-Injected": "yes"})
	}))
	ts.EnableHTTP2 = true
	if err = configureIngressHTTP2(ts.Config); err != nil {
		t.Fatal(err)
	}
	ts.StartTLS()
	defer ts.Close()
	tr := &http2.Transport{TLSClientConfig: ts.Client().Transport.(*http.Transport).TLSClientConfig}
	defer tr.CloseIdleConnections()
	client := &http.Client{Transport: tr, Timeout: time.Second}
	r, err := http.NewRequest("POST", ts.URL, strings.NewReader("streamed-body"))
	if err != nil {
		t.Fatal(err)
	}
	r.SetBasicAuth("u", "p")
	response, err := client.Do(r)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(response.Body)
	if err != nil || response.StatusCode != 200 || response.ProtoMajor != 2 || string(body) != "streamed-body" || response.Trailer.Get("Echo-Trailer") != "delivered" {
		t.Fatalf("HTTP/2→HTTP/1.1 failed: %v %+v %q", err, response, string(body))
	}
}

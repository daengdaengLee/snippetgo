package gracefulshutdown_test

import (
	"context"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"

	"github.com/daengdaengLee/snippetgo/gracefulshutdown"
)

// notifyListener 는 리스너가 닫히는 시점을 테스트에서 관측하기 위한 래퍼다.
//
// Serve 가 리스너를 주입받는 덕분에 이 래퍼를 끼울 수 있고, 그래서 테스트가
// "shutdown 이 리스너까지 닫았다"를 sleep 없이 정확한 시점에 알 수 있다.
type notifyListener struct {
	net.Listener

	once   sync.Once
	closed chan struct{}
}

// Close 는 실제 리스너를 먼저 닫은 뒤에 closed 를 닫는다.
// 순서가 반대이면 소켓이 아직 살아 있는 동안 Closed() 가 열려버려서,
// 새 연결이 거부되는지 검사하는 테스트가 간헐적으로 실패한다.
//
// sync.Once 가 필요한 이유는 Close 가 두 번 이상 불리기 때문이다.
// srv.Serve 의 defer 와 테스트 cleanup 양쪽에서 호출된다.
// net.TCPListener.Close 자체는 여러 번 불려도 안전하다.
func (l *notifyListener) Close() error {
	err := l.Listener.Close()
	l.once.Do(func() { close(l.closed) })
	return err
}

// Closed 는 리스너가 실제로 닫히면 닫히는 채널을 돌려준다.
func (l *notifyListener) Closed() <-chan struct{} { return l.closed }

// newListener 는 임의의 빈 포트에 바인딩한 관측 가능한 리스너를 만든다.
func newListener(t *testing.T) *notifyListener {
	t.Helper()

	inner, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	ln := &notifyListener{Listener: inner, closed: make(chan struct{})}
	// t.Cleanup 은 func() 을 받으므로 func() error 인 Close 를 그대로 넘길 수 없다.
	t.Cleanup(func() { _ = ln.Close() })
	return ln
}

// closeErrListener 는 Close 가 에러를 반환하는 리스너를 흉내 낸다.
//
// 내부 리스너는 실제로 먼저 닫는다. 그래야 Accept 가 깨어나 srv.Serve 가 반환하고
// Shutdown 의 listenerGroup.Wait() 가 풀린다. 에러는 그 뒤에 돌려준다.
// sentinel 은 이미 errors 를 import 하는 gracefulshutdown_test.go 가 소유한다.
type closeErrListener struct {
	net.Listener

	err error
}

func (l *closeErrListener) Close() error {
	_ = l.Listener.Close()
	return l.err
}

// newCloseErrListener 는 Close 가 err 를 반환하는 리스너를 만든다.
func newCloseErrListener(t *testing.T, err error) *closeErrListener {
	t.Helper()

	return &closeErrListener{Listener: newListener(t), err: err}
}

// newClient 는 테스트마다 새 Transport 를 가진 클라이언트를 만든다.
//
// 클라이언트를 공유하면 idle keep-alive 커넥션이 재사용되는데, 그러면
// "새 연결이 거부되는가"(T3)와 "drain 중에도 새 연결을 받는가"(T5) 검증이 무의미해진다.
func newClient(t *testing.T) *http.Client {
	t.Helper()

	c := &http.Client{Transport: &http.Transport{}}
	t.Cleanup(c.CloseIdleConnections)
	return c
}

// startServe 는 Serve 를 goroutine 으로 띄우고 결과를 받을 채널을 돌려준다.
func startServe(
	t *testing.T,
	ctx context.Context,
	srv *http.Server,
	ln net.Listener,
	cfg gracefulshutdown.Config,
) <-chan error {
	t.Helper()

	done := make(chan error, 1)
	go func() { done <- gracefulshutdown.Serve(ctx, srv, ln, cfg) }()
	return done
}

// urlFor 는 리스너 주소에 path 를 붙인 URL 을 만든다.
func urlFor(ln net.Listener, path string) string {
	return "http://" + ln.Addr().String() + path
}

// result 는 요청을 goroutine 으로 보낼 때 쓰는 값 전달용 구조체다.
// 판정은 항상 테스트 goroutine 이 한다.
type result struct {
	status int
	body   string
	err    error
}

// fetch 는 goroutine 에서 호출해도 안전하도록 *testing.T 를 받지 않는다.
//
// go vet 의 testinggoroutine 분석기가 테스트가 띄운 goroutine 안의 t.Fatal 호출을 막기 때문에,
// 요청을 백그라운드로 보내는 경로에서는 값만 넘기고 판정은 테스트 goroutine 에서 해야 한다.
func fetch(c *http.Client, url string) result {
	resp, err := c.Get(url)
	if err != nil {
		return result{err: err}
	}
	defer func() { _ = resp.Body.Close() }()

	// body 를 끝까지 읽어야 커넥션이 idle 로 돌아가 재사용되거나 제때 정리된다.
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return result{status: resp.StatusCode, err: err}
	}
	return result{status: resp.StatusCode, body: string(body)}
}

// getOK 는 테스트 goroutine 에서만 쓴다. 200 이 아니면 즉시 실패시킨다.
func getOK(t *testing.T, c *http.Client, url string) string {
	t.Helper()

	r := fetch(c, url)
	if r.err != nil {
		t.Fatalf("GET %s: %v", url, r.err)
	}
	if r.status != http.StatusOK {
		t.Fatalf("GET %s: status = %d, want %d", url, r.status, http.StatusOK)
	}
	return r.body
}

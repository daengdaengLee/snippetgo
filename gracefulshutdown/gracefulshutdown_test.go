package gracefulshutdown_test

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/daengdaengLee/snippetgo/gracefulshutdown"
)

// 이 테스트들은 time.Sleep 으로 "이쯤이면 됐겠지" 하고 넘어가지 않는다.
// 대신 세 가지 도구로 종료 절차를 원하는 지점에 정확히 고정한다.
//
//   - 리스너 래퍼로 "리스너가 실제로 닫힌 시점"을 관측한다.
//   - 핸들러를 채널로 블록시켜 in-flight 요청의 수명을 테스트가 통제한다.
//   - OnShutdownStart 훅을 블록시켜 drain 구간을 붙잡아 둔다.
//
// 핸들러 안에서 채널을 닫을 때는 sync.OnceFunc 으로 감싼다.
// 핸들러가 두 번 이상 불리면 close 가 패닉해 테스트 바이너리 전체가 죽기 때문이다.

// errCloseFailed 는 리스너 close 실패를 흉내 내는 sentinel 이다.
var errCloseFailed = errors.New("listener close failed on purpose")

// TestServe_ReturnsNilOnGracefulShutdown 은 정상 종료가 에러로 보고되지 않는지 본다.
// srv.Serve 는 종료 시 http.ErrServerClosed 를 돌려주는데, 그대로 흘리면
// 성공한 배포가 매번 실패로 기록된다.
func TestServe_ReturnsNilOnGracefulShutdown(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln := newListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{})

	if got := getOK(t, newClient(t), urlFor(ln, "/")); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}

	cancel()

	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
}

// TestServe_WaitsForInFlightRequest 는 이 패키지의 핵심 계약 두 가지를 한꺼번에 검증한다.
//
//   - 종료가 시작돼도 이미 처리 중인 요청은 끝까지 완료된다.
//   - 그 사이 핸들러의 r.Context() 는 살아 있다.
//
// 둘 중 하나라도 깨지면 이 테스트가 결정적으로 실패한다.
// shutdown 에서 context.WithoutCancel 을 빠뜨리면 리스너가 닫히는 순간 커넥션이 끊겨
// 응답을 못 받고, BaseContext 에서 빠뜨리면 r.Context().Err() 가 nil 이 아니게 된다.
func TestServe_WaitsForInFlightRequest(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	markEntered := sync.OnceFunc(func() { close(entered) })
	release := make(chan struct{})
	// 핸들러가 관측한 값은 공유 변수가 아니라 채널로 넘긴다.
	// 소켓 I/O 는 race detector 가 인정하는 동기화 간선이 아니다.
	ctxErrCh := make(chan error, 1)

	ln := newListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		markEntered()
		<-release
		ctxErrCh <- r.Context().Err()
		_, _ = io.WriteString(w, "done")
	})}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{})

	client := newClient(t)
	respCh := make(chan result, 1)
	go func() { respCh <- fetch(client, urlFor(ln, "/slow")) }()

	<-entered
	cancel()
	<-ln.Closed() // shutdown 이 리스너까지 닫았다. sleep 을 없애는 지점이다.

	// 핸들러가 release 를 기다리고 있으므로 응답이 왔을 리가 없다.
	// non-blocking 검사라 느린 머신에서도 거짓 실패가 나올 수 없다.
	select {
	case r := <-respCh:
		t.Fatalf("got a response before the handler finished: %+v", r)
	default:
	}

	close(release)

	r := <-respCh
	if r.err != nil {
		t.Fatalf("in-flight request failed: %v", r.err)
	}
	if r.status != http.StatusOK {
		t.Fatalf("status = %d, want %d", r.status, http.StatusOK)
	}
	if r.body != "done" {
		t.Fatalf("body = %q, want %q", r.body, "done")
	}
	if err := <-ctxErrCh; err != nil {
		t.Fatalf("r.Context() must stay alive during graceful shutdown: %v", err)
	}
	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
}

// TestServe_RejectsNewConnectionsAfterShutdownStarts 는 종료가 시작되면
// 새 연결이 더 이상 수립되지 않는지 본다.
//
// http.Client 가 아니라 raw dial 을 쓴다. 클라이언트는 살아 있는 idle 커넥션을
// 재사용할 수 있어서 "새 연결"을 검증하지 못한다.
func TestServe_RejectsNewConnectionsAfterShutdownStarts(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	markEntered := sync.OnceFunc(func() { close(entered) })
	release := make(chan struct{})

	ln := newListener(t)
	addr := ln.Addr().String() // 닫힌 리스너에서 읽지 않도록 미리 캡처한다
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		markEntered()
		<-release
		_, _ = io.WriteString(w, "done")
	})}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{})

	client := newClient(t)
	respCh := make(chan result, 1)
	go func() { respCh <- fetch(client, urlFor(ln, "/slow")) }()

	<-entered
	cancel()
	<-ln.Closed()

	// 리스너 래퍼가 실제 소켓을 먼저 닫고 채널을 나중에 닫으므로,
	// 여기서의 dial 은 커널이 곧바로 거부한다. 타임아웃은 상한일 뿐 소비되지 않는다.
	conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a new connection was accepted after shutdown started")
	}

	close(release)
	<-respCh

	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
}

// TestServe_ForceClosesAfterShutdownTimeout 은 유예 시간을 넘겼을 때의 동작을 본다.
//
// 핸들러가 hard stop 전에는 절대 반환하지 않으므로 머신이 아무리 느려도 유예는 항상 초과된다.
// 100ms 라는 값은 테스트 소요 시간에만 영향을 주고 판정에는 영향을 주지 않는다.
func TestServe_ForceClosesAfterShutdownTimeout(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	entered := make(chan struct{})
	markEntered := sync.OnceFunc(func() { close(entered) })
	observed := make(chan struct{})
	markObserved := sync.OnceFunc(func() { close(observed) })

	ln := newListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		markEntered()
		<-r.Context().Done() // hard stop 이 오기 전에는 끝나지 않는다
		markObserved()
	})}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{
		ShutdownTimeout: 100 * time.Millisecond,
	})

	client := newClient(t)
	respCh := make(chan result, 1)
	go func() { respCh <- fetch(client, urlFor(ln, "/hang")) }()

	<-entered
	cancel()
	<-observed // 유예가 끝나면서 hard stop 이 핸들러까지 전달됐다

	err := <-serveDone
	if !errors.Is(err, gracefulshutdown.ErrShutdownTimeout) {
		t.Fatalf("Serve() = %v, want error wrapping ErrShutdownTimeout", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Serve() = %v, want error wrapping context.DeadlineExceeded", err)
	}

	<-respCh // 요청 goroutine 회수
}

// TestServe_DrainDelayAndHook 은 종료 신호와 리스너 닫기 사이의 구간을 검증한다.
//
// 훅을 블록시켜 두면 종료 절차가 그 지점에서 멈추므로, 타이머 경합 없이
// "훅이 먼저 불렸고, 리스너는 아직 열려 있고, 새 연결도 받는다"를 확정적으로 관측할 수 있다.
func TestServe_DrainDelayAndHook(t *testing.T) {
	t.Parallel()

	const drainDelay = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	hookCalled := make(chan struct{})
	hookRelease := make(chan struct{})

	ln := newListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{
		DrainDelay: drainDelay,
		OnShutdownStart: func() {
			close(hookCalled)
			<-hookRelease
		},
	})

	if got := getOK(t, newClient(t), urlFor(ln, "/")); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}

	cancel()
	<-hookCalled

	select {
	case <-ln.Closed():
		t.Fatal("the hook must run before the listener is closed")
	default:
	}

	// 훅이 블록 중이라 종료가 진행되지 않는다. 이 구간에서도 새 연결을 받아야 한다.
	// 새 Transport 를 써야 idle 커넥션 재사용이 아니라 실제 신규 연결이 된다.
	if got := getOK(t, newClient(t), urlFor(ln, "/")); got != "ok" {
		t.Fatalf("body during drain = %q, want %q", got, "ok")
	}

	start := time.Now()
	close(hookRelease)
	<-ln.Closed()

	// 느린 머신은 elapsed 를 키우기만 하므로 이 단언은 거짓 실패 방향이 아니다.
	if elapsed := time.Since(start); elapsed < drainDelay {
		t.Fatalf("drain finished in %v, want at least %v", elapsed, drainDelay)
	}

	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
}

// TestServe_AbortDrainSkipsRemainingDelay 는 Config.AbortDrain 이 남은 유예를
// 건너뛰게 하는지 본다. 시그널을 쓸 수 없는 환경의 탈출구다.
//
// 상한 단언이 없으면 AbortDrain 을 아예 보지 않는 구현도 통과한다.
// 그런 구현은 타이머 만료까지 기다렸다가 결국 nil 을 돌려주기 때문이다.
func TestServe_AbortDrainSkipsRemainingDelay(t *testing.T) {
	t.Parallel()

	const drainDelay = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	abort := make(chan struct{})
	var start time.Time

	ln := newListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{
		DrainDelay: drainDelay,
		AbortDrain: abort,
		OnShutdownStart: func() {
			// 훅은 drain 진입 직전에 동기로 불린다. 여기서 닫으면 타이머 경합이 없다.
			start = time.Now()
			close(abort)
		},
	})

	if got := getOK(t, newClient(t), urlFor(ln, "/")); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}

	cancel()

	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed >= drainDelay {
		t.Fatalf("drain took %v, want less than %v", elapsed, drainDelay)
	}
}

// TestServe_DrainWakesWhenServeExits 는 drain 중에 srv.Serve 가 죽으면
// 남은 유예를 그대로 소진하지 않는지 본다. 새 연결을 이미 못 받는 상태이므로
// 계속 기다리는 것은 순수한 낭비다.
func TestServe_DrainWakesWhenServeExits(t *testing.T) {
	t.Parallel()

	const drainDelay = 10 * time.Second

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	var start time.Time

	ln := newListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{
		DrainDelay: drainDelay,
		OnShutdownStart: func() {
			// 훅 안에서 동기로 닫으면 drain 에 들어설 때 srv.Serve 는 이미 끝나 있다.
			start = time.Now()
			_ = srv.Close()
		},
	})

	if got := getOK(t, newClient(t), urlFor(ln, "/")); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}

	cancel()

	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed >= drainDelay {
		t.Fatalf("drain took %v, want less than %v", elapsed, drainDelay)
	}
}

// TestServe_ReturnsListenerError 는 종료 신호보다 먼저 srv.Serve 가 실패한 경우를 본다.
// Serve 가 ctx 취소를 무한정 기다리지 않고 그 에러를 그대로 돌려줘야 한다.
func TestServe_ReturnsListenerError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln := newListener(t)
	if err := ln.Close(); err != nil {
		t.Fatalf("ln.Close: %v", err)
	}

	srv := &http.Server{Handler: http.NotFoundHandler()}
	err := <-startServe(t, ctx, srv, ln, gracefulshutdown.Config{})
	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve() = %v, want error wrapping net.ErrClosed", err)
	}
}

// TestServe_CallsHookWhenServeFailsFirst 는 srv.Serve 가 종료 신호보다 먼저
// 실패한 경로에서도 훅이 불리는지 본다.
//
// 이 경로에서 훅을 건너뛰면 in-flight 를 비우는 내내 readiness 가 200 을 반환한다.
// readiness 를 별도 리스너로 서비스하는 구성에서는 LB 가 계속 트래픽을 보내고
// 주 리스너는 이미 닫혀 있어 502 가 난다. 이 패키지가 막으려던 바로 그 실패 모드다.
func TestServe_CallsHookWhenServeFailsFirst(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln := newListener(t)
	if err := ln.Close(); err != nil {
		t.Fatalf("ln.Close: %v", err)
	}

	hookCalled := make(chan struct{})
	srv := &http.Server{Handler: http.NotFoundHandler()}
	err := <-startServe(t, ctx, srv, ln, gracefulshutdown.Config{
		OnShutdownStart: func() { close(hookCalled) },
	})

	if !errors.Is(err, net.ErrClosed) {
		t.Fatalf("Serve() = %v, want error wrapping net.ErrClosed", err)
	}
	// 훅은 동기 호출이므로 Serve 가 반환한 시점에는 이미 끝나 있다.
	select {
	case <-hookCalled:
	default:
		t.Fatal("OnShutdownStart must run even when srv.Serve fails before the signal")
	}
}

// TestServe_ClosesListenerWhenHookPanics 는 사용자 코드인 훅이 panic 해도
// 서버가 열린 채로 남지 않는지 본다.
//
// 정리를 defer 로 보장하지 않으면 호출자가 recover 했을 때 프로세스는 살아 있는데
// 리스너도 열려 있고 hardStop 만 취소된 좀비 상태가 된다.
func TestServe_ClosesListenerWhenHookPanics(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln := newListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}

	// startServe 를 쓰면 panic 이 테스트 바이너리를 통째로 죽인다. 여기서 recover 한다.
	panicked := make(chan any, 1)
	go func() {
		defer func() { panicked <- recover() }()
		_ = gracefulshutdown.Serve(ctx, srv, ln, gracefulshutdown.Config{
			OnShutdownStart: func() { panic("boom") },
		})
	}()

	if got := getOK(t, newClient(t), urlFor(ln, "/")); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}

	cancel()
	<-ln.Closed() // 훅이 panic 해도 리스너는 닫힌다

	if r := <-panicked; r == nil {
		t.Fatal("the panic must propagate out of Serve")
	}
}

// TestServe_WrapsListenerCloseError 는 요청을 전부 비운 뒤 리스너를 닫다가 난 에러가
// ErrListenerClose 로 구분되는지 본다.
//
// 이 값을 그대로 흘리면 성공한 배포가 실패로 기록된다. 호출자가 errors.Is 로
// 구분할 수 있어야 exit code 를 옳게 정할 수 있다.
func TestServe_WrapsListenerCloseError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln := newCloseErrListener(t, errCloseFailed)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{})

	// 이 요청이 랑데부다. 없으면 srv.Serve 가 리스너를 등록하기 전에 Shutdown 이
	// 종료 플래그를 세워 closeListenersLocked 가 빈 맵을 돌고 lnerr 가 nil 이 된다.
	if got := getOK(t, newClient(t), urlFor(ln, "/")); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}

	cancel()

	err := <-serveDone
	if !errors.Is(err, gracefulshutdown.ErrListenerClose) {
		t.Fatalf("Serve() = %v, want error wrapping ErrListenerClose", err)
	}
	if !errors.Is(err, errCloseFailed) {
		t.Fatalf("Serve() = %v, want error wrapping errCloseFailed", err)
	}
	if errors.Is(err, gracefulshutdown.ErrShutdownTimeout) {
		t.Fatalf("Serve() = %v, must not be reported as a timeout", err)
	}
}

// TestServe_ReportsErrServerClosedOnReusedServer 는 종료 신호 없이 srv 가 닫혀 있을 때
// Serve 가 조용히 성공하지 않는지 본다.
//
// http.ErrServerClosed 를 무조건 nil 로 정규화하면, 이미 Shutdown 한 srv 를 다시 넘겨도
// 서버가 아무것도 하지 않은 채 nil 이 나와 진단이 매우 어려워진다.
func TestServe_ReportsErrServerClosedOnReusedServer(t *testing.T) {
	t.Parallel()

	// ctx 는 일부러 취소하지 않는다. 종료 신호 없이 srv 만 닫힌 상황을 만든다.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln := newListener(t)
	srv := &http.Server{Handler: http.NotFoundHandler()}
	if err := srv.Close(); err != nil {
		t.Fatalf("srv.Close: %v", err)
	}

	// trackListener 가 false 를 돌려주므로 srv.Serve 는 즉시 ErrServerClosed 를 반환한다.
	err := <-startServe(t, ctx, srv, ln, gracefulshutdown.Config{})
	if !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve() = %v, want error wrapping http.ErrServerClosed", err)
	}
}

type ctxKey struct{}

// TestServe_PreservesCallerBaseContext 는 호출자가 이미 BaseContext 를 설정한 경우를 본다.
// Serve 가 그것을 덮어써 버리면 호출자가 심어 둔 값이 사라지고,
// 반대로 체인만 하고 hard stop 을 얹지 않으면 강제 종료 신호가 핸들러까지 가지 않는다.
func TestServe_PreservesCallerBaseContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// 버퍼 1 이어야 한다. 무버퍼로 두면 핸들러는 send 에서 멈추고
	// 테스트는 다른 채널에서 기다리다가 서로를 붙잡는다.
	valCh := make(chan any, 1)
	observed := make(chan struct{})
	markObserved := sync.OnceFunc(func() { close(observed) })

	ln := newListener(t)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
			valCh <- r.Context().Value(ctxKey{})
			<-r.Context().Done()
			markObserved()
		}),
		BaseContext: func(net.Listener) context.Context {
			return context.WithValue(context.Background(), ctxKey{}, "caller")
		},
	}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{
		ShutdownTimeout: 100 * time.Millisecond,
	})

	client := newClient(t)
	respCh := make(chan result, 1)
	go func() { respCh <- fetch(client, urlFor(ln, "/hang")) }()

	// 값 수신이 곧 핸들러 진입 확인이다.
	if got := <-valCh; got != "caller" {
		t.Fatalf("r.Context().Value = %v, want %q", got, "caller")
	}

	cancel()
	<-observed // 체인된 context 로도 hard stop 이 전파됐다

	if err := <-serveDone; !errors.Is(err, gracefulshutdown.ErrShutdownTimeout) {
		t.Fatalf("Serve() = %v, want error wrapping ErrShutdownTimeout", err)
	}

	<-respCh
}

// TestServe_RestoresBaseContext 는 Serve 가 srv.BaseContext 를 원래대로 돌려놓는지 본다.
//
// hardStop 을 캡처한 클로저를 srv 에 남기면, 같은 srv 를 다시 넘겼을 때
// AfterFunc 이 즉시 실행되어 모든 요청 context 가 시작하자마자 취소된다.
// 서버는 정상 리스닝하는데 모든 핸들러의 r.Context() 가 처음부터 죽어 있는 상태라
// panic 도 에러도 없이 조용히 잘못 동작한다.
func TestServe_RestoresBaseContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln := newListener(t)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "ok")
	})}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{})

	if got := getOK(t, newClient(t), urlFor(ln, "/")); got != "ok" {
		t.Fatalf("body = %q, want %q", got, "ok")
	}

	cancel()

	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
	// 함수 값은 nil 과만 비교할 수 있으므로 BaseContext 를 설정하지 않은 srv 로 확인한다.
	if srv.BaseContext != nil {
		t.Fatal("Serve() must restore srv.BaseContext to its original value")
	}
}

// TestServe_AlreadyCanceledContext 는 이미 취소된 context 로 호출한 경우를 본다.
//
// 이때 srv.Serve 의 리스너 등록과 Shutdown 의 종료 플래그 설정이 경합해 두 갈래로 갈리지만,
// ctx 가 취소된 상태이므로 어느 쪽이든 Serve 는 http.ErrServerClosed 를 nil 로 정규화하고
// 리스너는 srv.Serve 의 defer 가 반드시 닫는다. 관측 가능한 결과는 하나뿐이다.
func TestServe_AlreadyCanceledContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	ln := newListener(t)
	srv := &http.Server{Handler: http.NotFoundHandler()}
	serveDone := startServe(t, ctx, srv, ln, gracefulshutdown.Config{})

	<-ln.Closed()

	if err := <-serveDone; err != nil {
		t.Fatalf("Serve() = %v, want nil", err)
	}
}

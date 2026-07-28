// Package gracefulshutdown 은 net/http 서버를 context 로 제어해 안전하게 종료하는 방법을 보여준다.
//
// 배포 중 502 가 나는 원인은 대부분 srv.Shutdown 을 몰라서가 아니라 그 주변의 함정들 때문이다.
// 이 패키지는 그중 코드로 풀 수 있는 여섯 가지를 다룬다.
//
//   - http.ErrServerClosed 는 정상 종료다. 에러로 보고하면 안 된다.
//   - Shutdown 은 유예 시간을 주지 않으면 무한히 기다린다.
//   - 이미 취소된 context 로 타임아웃을 파생시키면 graceful 이 통째로 무력화된다.
//   - Shutdown 은 in-flight 요청의 r.Context() 를 취소하지 않는다.
//   - Shutdown 은 리스너가 반환할 때까지 ctx 를 보지 않는다.
//   - 정상 완료 시의 리스너 close 에러는 배포 실패가 아니다.
//
// 나머지 함정(hijacked 커넥션, 백그라운드 워커 종료 순서 등)은 README.md 에 정리했다.
package gracefulshutdown

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"
)

// DefaultShutdownTimeout 은 Config.ShutdownTimeout 을 지정하지 않았을 때 쓰는 값이다.
const DefaultShutdownTimeout = 10 * time.Second

var (
	// ErrShutdownTimeout 은 ShutdownTimeout 안에 정리가 끝나지 않아
	// 남은 커넥션을 강제로 닫았을 때 반환된다. 유예를 넘기면 그 시각에
	// hard stop 이 전달되고 이 에러가 반환된다. in-flight 요청이 남은 경우뿐 아니라
	// 리스너 종료 대기가 유예를 넘긴 경우도 포함한다.
	//
	// 이 에러는 원인이 된 context.DeadlineExceeded 도 함께 감싸므로
	// errors.Is(err, ErrShutdownTimeout) 과 errors.Is(err, context.DeadlineExceeded) 가 모두 참이다.
	ErrShutdownTimeout = errors.New("gracefulshutdown: shutdown timed out")

	// ErrListenerClose 는 in-flight 요청을 모두 정상 처리한 뒤 리스너를 닫는
	// 과정에서만 난 에러를 감싼다. 요청은 전부 끝났으므로 배포 실패가 아니다.
	ErrListenerClose = errors.New("gracefulshutdown: listener close failed")
)

// Config 는 Serve 의 종료 동작을 조절한다. 제로 값도 그대로 쓸 수 있다.
type Config struct {
	// ShutdownTimeout 은 in-flight 요청이 끝나기를 기다리는 최대 시간이다.
	// 초과하면 핸들러의 r.Context() 를 취소하고 남은 커넥션을 강제로 닫는다.
	// 0 이하면 DefaultShutdownTimeout 을 쓴다.
	ShutdownTimeout time.Duration

	// DrainDelay 는 종료 신호를 받은 뒤 실제 shutdown 을 시작하기 전까지 기다리는 시간이다.
	// 로드밸런서나 Kubernetes Endpoints 에서 이 인스턴스가 빠질 시간을 벌어 준다.
	// 기다리는 동안 서버는 새 요청을 계속 받는다. 0 이하면 기다리지 않는다.
	// srv.Serve 가 반환하거나 AbortDrain 이 닫히면 남은 시간을 건너뛴다.
	DrainDelay time.Duration

	// OnShutdownStart 는 정리를 시작하기 직전에 동기적으로 호출된다.
	// ctx 취소든 srv.Serve 의 선행 종료든 정확히 한 번 불린다.
	// readiness probe 를 503 으로 전환하는 용도다.
	// 오래 블록하면 종료가 그만큼 늦어진다. nil 이면 호출하지 않는다.
	OnShutdownStart func()

	// AbortDrain 이 닫히면 남은 DrainDelay 를 건너뛰고 곧바로 shutdown 으로 넘어간다.
	// nil 이면 중단하지 않는다. drain 구간에 진입하는 시점에 이미 닫혀 있으면
	// DrainDelay 를 전부 건너뛰고, drain 이 끝난 뒤에 닫히는 것은 아무 효과가 없다.
	//
	// 시그널이 있는 프로세스는 두 번째 시그널로 프로세스를 끝내는 쪽이 간단하다(cmd 참고).
	// 이 필드는 시그널을 쓸 수 없는 환경, 즉 테스트나 라이브러리 임베딩을 위한 경로다.
	AbortDrain <-chan struct{}
}

// withDefaults 는 지정하지 않은 필드를 기본값으로 채운 복사본을 돌려준다.
func (c Config) withDefaults() Config {
	if c.ShutdownTimeout <= 0 {
		c.ShutdownTimeout = DefaultShutdownTimeout
	}
	return c
}

// Serve 는 ln 위에서 srv 를 실행하고, ctx 가 취소되면 graceful shutdown 을 수행한다.
//
// 종료 순서는 다음과 같다.
//
//  1. ctx 취소, 또는 srv.Serve 의 선행 반환을 감지한다.
//  2. Config.OnShutdownStart 를 호출한다. ctx 취소 경로라면 리스너는 아직 열려 있어 새 요청도 받는다.
//  3. Config.DrainDelay 만큼 기다린다. 이 구간의 요청도 정상 처리된다.
//     srv.Serve 가 끝나거나 Config.AbortDrain 이 닫히면 남은 시간을 건너뛴다.
//  4. 리스너를 닫고 in-flight 요청이 끝나기를 ShutdownTimeout 까지 기다린다.
//  5. 유예를 넘기면 핸들러의 r.Context() 를 취소한 뒤 남은 커넥션을 강제로 닫는다.
//
// ln 의 소유권은 Serve 가 가져간다. srv.Serve 가 반환할 때 닫히므로 호출자가 따로 닫을 필요는 없다.
//
// Serve 는 srv.BaseContext 를 덮어쓰고 반환할 때 원래 값으로 되돌린다.
// 기존 값이 있으면 그 위에 체인하므로 호출자가 심어 둔 값은 그대로 유지된다.
//
// 하나의 srv 로 Serve 를 동시에 또는 반복해서 호출하면 안 된다.
// net/http 자체가 Shutdown 이후의 서버 재사용을 허용하지 않는다.
//
// 요청을 다 비우고 리스너까지 문제없이 닫히면 nil 을 반환한다.
// 요청은 다 비웠지만 리스너 close 가 실패하면 ErrListenerClose 로 감싼다.
// 종료 신호 전에 온 http.ErrServerClosed 는 정규화하지 않고 그대로 반환하므로,
// 이 패키지로 종료를 관장하려면 반드시 ctx 로 신호해야 한다.
//
// hijack 된 커넥션의 r.Context() 는 두 시점에 죽는다. 핸들러가 ServeHTTP 를 반환하면
// net/http 가 그 즉시 취소하고, hijack 후에도 계속 돌고 있으면 Serve 반환 시 hard stop 이
// 취소한다. 어느 쪽이든 커넥션 수명보다 먼저 죽으므로, hijack 하는 핸들러는 r.Context() 대신
// 애플리케이션이 소유한 context 를 써야 한다.
//
// Serve 는 srv.Serve 가 반환할 때까지 기다린다. ln.Close() 가 Accept 를 깨우지 못하는
// 리스너에서는 ShutdownTimeout 이 반환 시각을 묶지 못하고, srv.Close 도 같은
// listenerGroup 을 기다리므로 타임아웃 처리 경로 전체가 함께 멈춘다. 코드가 보장하는 것은
// 유예가 끝나는 시각에 hard stop 이 전달되고 그 사실이 반환값에 반영된다는 것뿐이다.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, cfg Config) error {
	cfg = cfg.withDefaults()

	// hard stop 은 유예가 끝나는 시각에 취소되어 in-flight 핸들러에게 "지금 즉시 중단"을 알린다.
	// 부모의 취소는 WithoutCancel 로 끊는다. 그러지 않으면 종료 신호가 오는 순간
	// 모든 r.Context() 가 죽어 graceful 이 아니게 된다. 값(trace, logger 등)은 그대로 남는다.
	hardStop, stopHard := context.WithCancel(context.WithoutCancel(ctx))

	// 호출자가 이미 BaseContext 를 설정했다면 그 위에 hard stop 을 얹는다.
	// base == nil 판정은 Serve 진입 시 확정되므로 클로저 밖에서 한 번만 한다.
	base := srv.BaseContext
	if base == nil {
		srv.BaseContext = func(net.Listener) context.Context { return hardStop }
	} else {
		srv.BaseContext = func(l net.Listener) context.Context {
			connCtx, cancel := context.WithCancel(base(l))
			// BaseContext 는 srv.Serve 호출당 한 번만 불리므로 AfterFunc 등록도 한 번뿐이다.
			context.AfterFunc(hardStop, cancel) // hardStop 취소를 호출자 context 로 전파한다
			return connCtx
		}
	}

	// serveErr 는 close(serveDone) 으로 공개한다. 쓰기는 close 앞, 읽기는 수신 뒤라 안전하다.
	// 닫힌 채널은 몇 번이든 수신할 수 있어 "이미 읽었는가" 를 추적할 상태가 필요 없다.
	var serveErr error
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		serveErr = classifyServeError(ctx, srv.Serve(ln))
	}()

	// OnShutdownStart 는 사용자 코드다. panic 으로 빠져나가도 서버는 반드시 닫는다.
	// 이 defer 는 순서가 전부 의미를 갖는다.
	//
	//   - stopHard 가 맨 앞이어야 어느 경로에서든 핸들러가 강제 close 보다 먼저 정리 기회를 받는다.
	//   - Close 는 <-serveDone 보다 먼저여야 한다. 훅이 panic 하면 리스너가 아직 열려 있어
	//     srv.Serve 가 Accept 에 묶여 있고, 이 Close 만이 그것을 깨운다.
	//   - BaseContext 복원은 srv.Serve 가 끝난 뒤여야 한다. 아직 살아 있으면
	//     BaseContext 를 읽는 쪽과 쓰는 쪽이 겹쳐 data race 가 된다.
	defer func() {
		stopHard()
		_ = srv.Close() // 타임아웃 경로에선 남은 커넥션을 실제로 끊는 강제 종료 수단이다. 정상 경로에선 no-op.
		<-serveDone // 훅 panic 등으로 본문 말미에 도달하지 못한 경로용
		srv.BaseContext = base
	}()

	select {
	case <-serveDone:
		// 종료 신호보다 먼저 Serve 가 끝났다. 리스너 장애 등 비정상 종료다.
		// 새 연결은 못 받지만 처리 중인 요청은 남아 있을 수 있으므로 정리는 그대로 한다.
	case <-ctx.Done():
	}

	// readiness 를 먼저 내린다. Serve 가 먼저 죽은 경로에서도 마찬가지다.
	// in-flight 를 비우는 동안 readiness 가 200 이면 LB 가 계속 트래픽을 보내 502 가 난다.
	if cfg.OnShutdownStart != nil {
		cfg.OnShutdownStart()
	}

	waitDrain(cfg, serveDone)
	shutdownErr := shutdown(ctx, srv, cfg, stopHard)

	<-serveDone // 반환값 계산이 defer 보다 먼저다. serveErr 를 읽기 전에 여기서 기다린다
	return errors.Join(serveErr, shutdownErr)
}

// waitDrain 은 LB 나 Endpoints 가 이 인스턴스를 빼갈 시간을 준다.
// DrainDelay 만큼 기다리되, 그 사이 srv.Serve 가 끝나거나
// Config.AbortDrain 이 닫히면 곧바로 돌아온다.
func waitDrain(cfg Config, serveDone <-chan struct{}) {
	if cfg.DrainDelay <= 0 {
		return
	}

	timer := time.NewTimer(cfg.DrainDelay)
	defer timer.Stop()

	select {
	case <-timer.C:
	case <-cfg.AbortDrain: // nil 채널은 영원히 블록되므로 미설정 시 무시된다
	case <-serveDone: // 리스너가 이미 죽었으면 새 연결도 못 받는다. 더 기다릴 이유가 없다
	}
}

// shutdown 은 유예 시간을 건 srv.Shutdown 을 수행하고, 마감 시각에 hard stop 을 전달한다.
// 남은 커넥션을 실제로 끊는 것은 Serve 의 정리 defer 다.
func shutdown(parent context.Context, srv *http.Server, cfg Config, stopHard context.CancelFunc) error {
	// parent 는 보통 이미 취소된 상태다. WithTimeout(parent, d) 로 만들면 파생 context 가
	// 즉시 취소되어 Shutdown 이 곧바로 반환하고 graceful 이 아니게 된다.
	// WithoutCancel 로 취소만 끊고 값은 유지한다.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(parent), cfg.ShutdownTimeout)
	defer cancel()

	// 유예가 끝나는 시각에 hard stop 을 건다. net/http 는 리스너가 반환할 때까지 ctx 를
	// 보지 않으므로(listenerGroup.Wait), 이 등록이 없으면 그 구간에서 유예가 끝나도
	// 핸들러가 끝까지 알지 못한다.
	// cancel 보다 나중에 등록해야 LIFO 로 먼저 돌아, 정상 종료 뒤의 cancel() 이
	// hard stop 을 오발동시키지 않는다.
	stop := context.AfterFunc(ctx, stopHard)
	defer stop()

	err := srv.Shutdown(ctx)

	// net/http 의 폴링 루프는 closeIdleConns() 를 ctx.Done() 보다 먼저 검사한다.
	// 그래서 마감이 지나 hard stop 이 in-flight 를 끊어 낸 덕분에 quiescent 가 되면
	// Shutdown 은 ctx.Err() 가 아니라 nil 을 돌려준다. 요청을 강제로 끊고도 성공으로
	// 보고하게 되므로 마감 여부는 여기서 직접 본다.
	// ctx 는 WithoutCancel 부모라 defer cancel() 전에는 마감 말고 취소될 길이 없다.
	if err == nil && ctx.Err() != nil {
		err = ctx.Err()
	}

	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: %w", ErrShutdownTimeout, err)
	case err != nil:
		// 남은 err 는 리스너를 닫으면서 난 것뿐이다. 요청은 모두 정상 처리됐다.
		return fmt.Errorf("%w: %w", ErrListenerClose, err)
	default:
		return nil
	}
}

// classifyServeError 는 http.ErrServerClosed 를 정상 종료로 보고 nil 로 바꾼다.
// 단 종료 신호가 오기 전이라면 ctx 취소 없이 srv 가 닫혔다는 뜻이므로 그대로 알린다.
// 이미 Shutdown 된 srv 를 넘겨받았거나, 호출자가 실행 중에 srv.Shutdown/srv.Close 를
// 직접 불렀거나 둘 중 하나다. 어느 쪽이든 Serve 가 관장하지 않은 종료다.
func classifyServeError(ctx context.Context, err error) error {
	if errors.Is(err, http.ErrServerClosed) && ctx.Err() != nil {
		return nil
	}
	return err
}

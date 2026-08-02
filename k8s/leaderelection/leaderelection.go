// Package leaderelection 은 Kubernetes Lease 오브젝트로 여러 인스턴스 중 하나만
// "리더" 로 선출하는 방법을 보여준다.
//
// 같은 앱을 여러 replica 로 띄웠을 때, 크론/리컨실러/싱글톤 워커처럼 "동시에 하나만"
// 돌아야 하는 작업이 있다. Kubernetes 는 이를 위한 분산 락 프리미티브로 coordination.k8s.io
// 의 Lease 오브젝트를 제공하고, client-go 의 tools/leaderelection 이 그 위에서
// 임대(lease) 획득 - 갱신(renew) - 상실(lose) 생명주기를 대신 굴려준다.
//
// 이 패키지는 그 client-go 리더 선출을 얇게 감싸, 콜백 세 개(OnStartedLeading /
// OnStoppedLeading / OnNewLeader) 만 채우면 되도록 정리한 것이다. 직접 Lease 를
// GET/UPDATE 하지 않는다 - 모든 경쟁과 만료 판단은 client-go 가 처리한다.
//
// 주의: 리더 선출은 "부드러운" 보장이다. 시계 오차나 네트워크 지연으로 옛 리더가
// 자기 상실을 늦게 인지하면 아주 짧은 순간 리더가 둘일 수 있다. 진짜 상호 배제가
// 필요한 자원은 리더 신분과 별개로 fencing(예: 리소스 버전/토큰 검증) 을 걸어야 한다.
// 자세한 함정은 README.md 에 정리했다.
package leaderelection

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	// 이 패키지 이름과 같아서 별칭을 준다. k8sle 로 시작하면 client-go 쪽임이 분명해진다.
	k8sle "k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// 기본 임대 타이밍. client-go 가 권장하는 값을 그대로 쓴다.
//
// 관계식은 항상 RetryPeriod * JitterFactor(1.2) < RenewDeadline < LeaseDuration
// 이어야 한다(단순 대소가 아니라 지터를 곱한 값과 비교한다).
// LeaseDuration 은 리더가 죽은 뒤 다른 인스턴스가 임대를 빼앗기까지의 상한(=failover
// 지연) 이고, RenewDeadline 은 현재 리더가 이 시간 안에 갱신에 실패하면 스스로
// 리더직을 내려놓는 기준이다. RetryPeriod 는 획득/갱신 재시도 간격이다.
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// ErrInvalidConfig 는 Namespace 나 LeaseName, Identity 처럼 기본값을 정할 수 없는
// 필수 값이 비어 있을 때 Run 이 반환한다. 어느 필드가 비었는지는 감싼 메시지로 알린다
// (errors.Is 로는 그대로 잡힌다).
var ErrInvalidConfig = errors.New("leaderelection: 필수 설정이 비었다")

// ErrLostLease 는 이 인스턴스가 리더였다가 갱신(renew) 에 실패해 리더직을 비자발적으로
// 상실했을 때 Run 이 반환한다. ctx 취소로 인한 정상 종료(nil 반환) 와 구분하기 위한 신호다.
// controller-runtime 처럼 이 에러를 받으면 프로세스를 종료해 재시작에 맡기거나(exit),
// RunUntilCancelled 로 감싸 같은 프로세스에서 재경쟁하게(rejoin) 할 수 있다.
var ErrLostLease = errors.New("leaderelection: 리더직을 상실했다 (갱신 실패)")

// Config 는 리더 선출 한 판에 필요한 설정이다.
//
// Namespace/LeaseName/Identity 는 필수이고, 나머지 타이밍 값은 0 이하면
// Default* 상수로 채워진다. 콜백은 nil 이어도 된다(아무 것도 하지 않음).
type Config struct {
	// Namespace 는 Lease 오브젝트가 생성/조회되는 네임스페이스다.
	Namespace string
	// LeaseName 은 경쟁하는 인스턴스들이 공유하는 Lease 오브젝트 이름이다.
	// 같은 리더 그룹에 속하려면 모두 같은 값을 써야 한다.
	LeaseName string
	// Identity 는 이 인스턴스의 고유 식별자다. Pod 안이라면 보통 Pod 이름
	// (Downward API 의 metadata.name) 을 쓴다. 그룹 안에서 유일해야 한다.
	Identity string

	// LeaseDuration/RenewDeadline/RetryPeriod 는 0 이하면 Default* 로 대체된다.
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration

	// WaitForLeaderWork 가 true 면, 리더 작업(OnStartedLeading) 이 시작된 경우 Run 은 그
	// 종료까지 기다린 뒤 반환한다. 리더가 된 적 없으면 기다리지 않고, 리더직 상실로 끝난
	// 경우(ctx 는 살아 있다) 에는 대기가 보장된다 - 건너뛰는 경로는 ctx 가 이미 죽어 다음
	// 판 자체가 없는 경우뿐이다.
	//
	// 이 대기가 주는 것은 둘이다.
	//   - 드레인: 종료 경로(SIGTERM 등) 에서도 리더 작업이 시작됐었다면 기다린다. 그래서
	//     "Run 이 반환했다 = 리더 작업이 끝났다" 가 성립한다. 끄면 Run 반환 직후 프로세스를
	//     끝낼 때 리더 작업이 진행 중인 채로 사라질 수 있다.
	//   - 인프로세스 겹침 방지: 같은 프로세스에서 RunUntilCancelled 이 곧바로 시작하는 다음
	//     OnStartedLeading 이 이전 리더 작업과 겹치지 않는다.
	//
	// 막아 주지 않는 것은 다른 프로세스와의 겹침이다. Lease 반납과 OnStoppedLeading 은
	// client-go 안에서 이 대기보다 먼저 끝나므로, 다음 리더는 이전 리더 작업이 정리를
	// 끝내기 전에 리더가 될 수 있다(범위와 대가는 README 함정 5).
	//
	// 기본값 false 는 client-go 동작(콜백을 별도 goroutine 으로 띄우고 기다리지 않음) 을
	// 그대로 노출한다. true 로 켜면 콜백이 넘어온 ctx 취소를 존중하지 않을 때 Run 이 무한히
	// 블록하므로, 콜백이 ctx 를 확실히 따르는 경우에만 켤 것.
	WaitForLeaderWork bool

	// OnStartedLeading 은 이 인스턴스가 리더가 됐을 때 호출된다. 넘어온 ctx 는
	// 리더직을 잃거나 상위 ctx 가 취소되면 함께 취소되므로, 리더 전용 작업 루프는
	// 이 ctx 를 존중해 종료해야 한다. 이 함수는 블록해도 되고, 반환하거나 ctx 가
	// 취소되면 리더 작업이 끝난 것으로 본다.
	OnStartedLeading func(ctx context.Context)
	// OnStoppedLeading 은 리더 전용 자원을 정리하는 자리다. 단, client-go 계약상 이 콜백은
	// 선출 루프에 진입한 뒤 종료할 때 항상 호출된다 - 이 인스턴스가 리더가 된 적 없어도
	// (팔로워가 종료해도) 불린다. 설정이 잘못돼 Run 이 선출 루프 진입 전에 반환하는 경우
	// (ErrInvalidConfig, 타이밍 관계식 위반) 는 예외로, 이때는 불리지 않는다.
	// OnStartedLeading 이 먼저 불렸다고 가정하지 말고, 정리 로직은 멱등이어야 한다.
	// 또 이 콜백은 WaitForLeaderWork 대기보다 먼저 불리므로, 여기서 닫는 자원을 리더 작업
	// goroutine 이 아직 쓰고 있을 수 있다.
	OnStoppedLeading func()
	// OnNewLeader 는 관찰된 리더가 바뀔 때마다 호출된다(자기 자신이 리더가 된
	// 경우 포함). 로깅/관측용으로 유용하다.
	OnNewLeader func(identity string)
}

// applyDefaults 는 비어 있는 타이밍 값을 Default* 상수로 채운 사본을 돌려준다.
// 순수 함수라 클러스터 없이 단위 테스트할 수 있다.
func (c Config) applyDefaults() Config {
	if c.LeaseDuration <= 0 {
		c.LeaseDuration = DefaultLeaseDuration
	}
	if c.RenewDeadline <= 0 {
		c.RenewDeadline = DefaultRenewDeadline
	}
	if c.RetryPeriod <= 0 {
		c.RetryPeriod = DefaultRetryPeriod
	}
	return c
}

// validate 는 기본값으로 못 채우는 필수 값만 확인한다.
//
// 타이밍 관계식(RetryPeriod < RenewDeadline < LeaseDuration, JitterFactor 포함) 은
// client-go NewLeaderElector 가 최종 권위이므로 여기서 중복 검사하지 않는다. 어긋나면
// Run 안의 NewLeaderElector 가 에러를 내고 Run 이 그대로 반환한다.
func (c Config) validate() error {
	switch {
	case c.Namespace == "":
		return fmt.Errorf("%w: Namespace", ErrInvalidConfig)
	case c.LeaseName == "":
		return fmt.Errorf("%w: LeaseName", ErrInvalidConfig)
	case c.Identity == "":
		return fmt.Errorf("%w: Identity", ErrInvalidConfig)
	}
	return nil
}

// Run 은 리더 선출 한 세션에 참여하고, 아래 둘 중 먼저 오는 시점에 반환한다.
//
//   - ctx 가 취소됨(예: SIGTERM): nil 을 반환한다. ctx 가 데드라인 만료 등 취소가 아닌
//     사유로 끝났다면 해당 ctx 에러를 그대로 반환한다.
//   - 리더직 비자발적 상실(갱신 실패): ctx 는 살아 있는데 client-go 의 선출 루프가
//     리더 임대를 놓쳐 반환한 경우로, ErrLostLease 를 반환한다.
//
// 어느 경로로 끝나든 이 인스턴스가 리더였다면 Lease 반납을 시도한다(ReleaseOnCancel). 다음
// 리더가 LeaseDuration 만료를 기다리지 않아 failover 가 빨라진다 - 조건은 README 함정 3.
//
// cfg.WaitForLeaderWork 가 true 면 위 시점에 더해, 리더 작업이 시작됐었던 경우 그 종료까지
// 기다린 뒤 반환한다(반환값은 대기 전에 확정되므로, 기다리는 동안 ctx 가 취소돼도 비자발적
// 상실은 그대로 ErrLostLease 로 보고된다).
//
// 설정이 잘못된 경우 ErrInvalidConfig 등을 반환한다. 리더직을 잃어도 같은 프로세스에서
// 계속 재경쟁하려면 RunUntilCancelled 를 쓴다.
func Run(ctx context.Context, client kubernetes.Interface, cfg Config) error {
	cfg = cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return err
	}

	// WaitForLeaderWork 용 신호. leaderWorkDone 은 OnStartedLeading 이 반환할 때 닫히고,
	// leaderWorkStarted 는 그 콜백이 시작되기라도 했는지를 알려준다. 리더가 된 적 없으면
	// leaderWorkDone 은 영영 닫히지 않으므로 started 를 먼저 확인해야 한다.
	var leaderWorkStarted atomic.Bool
	leaderWorkDone := make(chan struct{})

	// Lease 타입 락. EventRecorder 를 nil 로 두면 이벤트를 남기지 않으므로
	// RBAC 에 events 권한이 필요 없다(leases 권한만 있으면 된다).
	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Namespace: cfg.Namespace,
			Name:      cfg.LeaseName,
		},
		Client: client.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: cfg.Identity,
		},
	}

	elector, err := k8sle.NewLeaderElector(k8sle.LeaderElectionConfig{
		Lock: lock,
		// 선출 동작에는 영향이 없고 식별용으로만 쓰인다 - client-go 의 메트릭 라벨과
		// WatchDog(Check) 실패 메시지("failed election to renew leadership on lease %s").
		// 비워 두면 함정 8 대로 WatchDog 을 붙였을 때 그 메시지의 이름이 빈칸으로 나온다.
		Name: cfg.LeaseName,
		// 이름은 "OnCancel" 이지만 client-go 는 renew 루프를 빠져나온 뒤 종료 사유를 구분하지
		// 않고 반납을 시도한다 - 비자발적 상실에서도 돈다. failover 를 빠르게 하려고 켜되,
		// 반납 자체는 best effort 로 본다(README 함정 3).
		ReleaseOnCancel: true,
		LeaseDuration:   cfg.LeaseDuration,
		RenewDeadline:   cfg.RenewDeadline,
		RetryPeriod:     cfg.RetryPeriod,
		Callbacks: k8sle.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
				leaderWorkStarted.Store(true)
				defer close(leaderWorkDone)
				if cfg.OnStartedLeading != nil {
					cfg.OnStartedLeading(ctx)
				}
			},
			OnStoppedLeading: func() {
				if cfg.OnStoppedLeading != nil {
					cfg.OnStoppedLeading()
				}
			},
			OnNewLeader: func(identity string) {
				if cfg.OnNewLeader != nil {
					cfg.OnNewLeader(identity)
				}
			},
		},
	})
	if err != nil {
		return err
	}

	// elector.Run 은 ctx 취소 "또는" 리더직 상실 중 먼저 오는 시점에 반환한다
	// (client-go docstring: "stopped by ctx or it has stopped holding the leader lease").
	// 자체적으로 에러를 돌려주지 않으므로 종료 원인은 ctx 상태로 구분한다.
	elector.Run(ctx)

	// 종료 사유는 반드시 여기서 - 아래 대기보다 먼저 - 확정한다. WaitForLeaderWork 대기는
	// 리더 작업이 정리에 쓰는 시간만큼 길어질 수 있고, 그 사이 상위 ctx 가 취소되면
	// ctx.Err() 이 non-nil 로 바뀌어 비자발적 상실이 정상 종료(nil)로 둔갑한다.
	//
	// 이 순서를 결정적으로 검증하는 테스트는 없다 - 캡처와 취소 사이에 관측 가능한
	// happens-before 엣지가 없어(래퍼가 내부 상태를 노출하지 않는다) 테스트가 타이밍 마진에
	// 의존하게 된다. 그래서 코드 배치로만 보장한다. 이 줄을 대기 아래로 옮기지 말 것.
	exitErr := ctx.Err()

	// client-go 는 OnStartedLeading 을 별도 goroutine 으로 띄우고 그 종료를 기다리지
	// 않는다. 옵션이 켜져 있으면 여기서 기다려 둘을 보장한다 - 종료 경로에서는 "Run 반환 =
	// 리더 작업 종료"(드레인) 이고, 같은 프로세스가 Run 반환 뒤 곧바로 재경쟁할 때는 이전
	// 리더 작업이 다음 판까지 흐르지 않는다.
	//
	// exitErr == nil 은 acquire 가 성공했었다는 뜻이다 - client-go 의 acquire 는 상위 ctx
	// 가 죽어야만 false 를 반환하기 때문이다. 그러면 OnStartedLeading goroutine 도 이미 떴으니
	// leaderWorkDone 은 반드시 닫힌다. 즉 이 옵션이 존재하는 이유인 상실 -> 재경쟁 경로는
	// 플래그를 보지 않아도 대기가 보장된다.
	//
	// leaderWorkStarted 는 나머지 경로(상위 ctx 취소) 만 담당한다. 거기서는 acquire 직후
	// 취소되면 renew 가 wait.BackoffUntil 진입부의 ctx 검사에서 곧바로 반환해, goroutine 이
	// 플래그를 세우기 전에 elector.Run 이 반환할 수 있다. 그 창은 상위 ctx 가 이미 죽어
	// RunUntilCancelled 이 다음 판을 열지 않으므로, 재경쟁 겹침이 아니라 "Run 반환 뒤 리더
	// 작업 시작" 이라는 성격이다.
	if cfg.WaitForLeaderWork && (exitErr == nil || leaderWorkStarted.Load()) {
		<-leaderWorkDone
	}

	if exitErr == nil {
		return ErrLostLease // ctx 는 살아 있는데 반환됨 = 비자발적 리더직 상실
	}
	if errors.Is(exitErr, context.Canceled) {
		return nil // SIGTERM 등 정상 취소
	}
	return exitErr // 데드라인 만료 등은 그대로 알린다
}

// RunUntilCancelled 는 ctx 가 취소될 때까지 리더 선출에 계속 참여한다.
//
// Run 을 반복 호출해, 리더직을 비자발적으로 잃으면(ErrLostLease) 같은 프로세스에서
// 곧바로 후보로 재참여(재경쟁)한다. ctx 가 취소(Canceled)되면 nil 을 반환하고,
// 데드라인 만료 등 그 외 ctx 종료 사유나 설정 오류처럼 ErrLostLease 가 아닌 에러는
// 재시도하지 않고 그대로 반환한다.
//
// 주의: 리더가 잡고 있던 in-memory 상태가 남은 채 재경쟁하므로, 리더 전용 작업은
// OnStartedLeading 의 ctx 취소를 존중해 확실히 정리한 뒤 다음 리더로 넘어가야 한다.
// 이전 리더 작업이 끝난 뒤에만 다음 판을 시작하고 싶으면 cfg.WaitForLeaderWork 를 켠다.
func RunUntilCancelled(ctx context.Context, client kubernetes.Interface, cfg Config) error {
	for {
		err := Run(ctx, client, cfg)
		if err == nil {
			return nil // ctx 취소로 정상 종료
		}
		if !errors.Is(err, ErrLostLease) {
			return err // 설정 오류 등은 재시도하지 않는다
		}
		// 상실과 ctx 종료가 겹친 경우. Run 과 같은 규칙으로 사유를 구분한다.
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.Canceled) {
				return nil // 정상 취소
			}
			return ctxErr // 데드라인 만료 등은 그대로 알린다
		}
		// 리더직만 잃고 ctx 는 살아 있음 -> 루프가 다시 Run 을 돌려 재경쟁(acquire)한다.
	}
}

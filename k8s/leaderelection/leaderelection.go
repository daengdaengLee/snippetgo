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
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// 기본 임대 타이밍. client-go 가 권장하는 값을 그대로 쓴다.
//
// 관계식은 항상 RetryPeriod < RenewDeadline < LeaseDuration 이어야 한다.
// LeaseDuration 은 리더가 죽은 뒤 다른 인스턴스가 임대를 빼앗기까지의 상한(=failover
// 지연) 이고, RenewDeadline 은 현재 리더가 이 시간 안에 갱신에 실패하면 스스로
// 리더직을 내려놓는 기준이다. RetryPeriod 는 획득/갱신 재시도 간격이다.
const (
	DefaultLeaseDuration = 15 * time.Second
	DefaultRenewDeadline = 10 * time.Second
	DefaultRetryPeriod   = 2 * time.Second
)

// ErrInvalidConfig 는 Namespace 나 LeaseName, Identity 처럼 기본값을 정할 수 없는
// 필수 값이 비어 있을 때 Run 이 반환한다.
var ErrInvalidConfig = errors.New("leaderelection: Namespace, LeaseName, Identity 는 필수다")

// ErrLostLease 는 이 인스턴스가 리더였다가 갱신(renew) 에 실패해 리더직을 비자발적으로
// 상실했을 때 Run 이 반환한다. ctx 취소로 인한 정상 종료(nil 반환) 와 구분하기 위한 신호다.
// controller-runtime 처럼 이 에러를 받으면 프로세스를 종료해 재시작에 맡기거나(exit),
// RunUntilCancelled 로 감싸 같은 프로세스에서 재경쟁하게(rejoin) 할 수 있다.
var ErrLostLease = errors.New("leaderelection: 리더직을 상실했다 (갱신 실패)")

// Config 는 리더 선출 한 판에 필요한 설정이다.
//
// Namespace/LeaseName/Identity 는 필수이고, 나머지 타이밍 값은 0 이면
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

	// LeaseDuration/RenewDeadline/RetryPeriod 는 0 이면 Default* 로 대체된다.
	LeaseDuration time.Duration
	RenewDeadline time.Duration
	RetryPeriod   time.Duration

	// OnStartedLeading 은 이 인스턴스가 리더가 됐을 때 호출된다. 넘어온 ctx 는
	// 리더직을 잃거나 상위 ctx 가 취소되면 함께 취소되므로, 리더 전용 작업 루프는
	// 이 ctx 를 존중해 종료해야 한다. 이 함수는 블록해도 되고, 반환하거나 ctx 가
	// 취소되면 리더 작업이 끝난 것으로 본다.
	OnStartedLeading func(ctx context.Context)
	// OnStoppedLeading 은 리더 전용 자원을 정리하는 자리다. 단, client-go 계약상 이 콜백은
	// Run 이 끝날 때 항상 호출된다 - 이 인스턴스가 리더가 된 적 없어도(팔로워가 종료해도)
	// 불린다. OnStartedLeading 이 먼저 불렸다고 가정하지 말고, 정리 로직은 멱등이어야 한다.
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
	if c.Namespace == "" || c.LeaseName == "" || c.Identity == "" {
		return ErrInvalidConfig
	}
	return nil
}

// Run 은 리더 선출 한 세션에 참여하고, 아래 둘 중 먼저 오는 시점에 반환한다.
//
//   - ctx 가 취소됨(예: SIGTERM): 이 인스턴스가 리더였다면 Lease 를 즉시 반납하고
//     (ReleaseOnCancel) nil 을 반환한다. 다음 리더는 LeaseDuration 만료를 기다리지 않고
//     바로 이어받아 failover 가 빨라진다. ctx 가 데드라인 만료 등 취소가 아닌 사유로
//     끝났다면 해당 ctx 에러를 그대로 반환한다.
//   - 리더직 비자발적 상실(갱신 실패): ctx 는 살아 있는데 client-go 의 선출 루프가
//     리더 임대를 놓쳐 반환한 경우로, ErrLostLease 를 반환한다.
//
// 설정이 잘못된 경우 ErrInvalidConfig 등을 반환한다. 리더직을 잃어도 같은 프로세스에서
// 계속 재경쟁하려면 RunUntilCancelled 를 쓴다.
func Run(ctx context.Context, client kubernetes.Interface, cfg Config) error {
	cfg = cfg.applyDefaults()
	if err := cfg.validate(); err != nil {
		return err
	}

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

	elector, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            lock,
		ReleaseOnCancel: true,
		LeaseDuration:   cfg.LeaseDuration,
		RenewDeadline:   cfg.RenewDeadline,
		RetryPeriod:     cfg.RetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(ctx context.Context) {
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
	if err := ctx.Err(); err != nil {
		if errors.Is(err, context.Canceled) {
			return nil // SIGTERM 등 정상 취소
		}
		return err // 데드라인 만료 등은 그대로 알린다
	}
	return ErrLostLease // ctx 는 살아 있는데 반환됨 = 비자발적 리더직 상실
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
func RunUntilCancelled(ctx context.Context, client kubernetes.Interface, cfg Config) error {
	for {
		err := Run(ctx, client, cfg)
		if err == nil {
			return nil // ctx 취소로 정상 종료
		}
		if !errors.Is(err, ErrLostLease) {
			return err // 설정 오류 등은 재시도하지 않는다
		}
		if ctx.Err() != nil {
			return nil // 상실과 취소가 겹친 경우도 정상 종료로 본다
		}
		// 리더직만 잃고 ctx 는 살아 있음 -> 루프가 다시 Run 을 돌려 재경쟁(acquire)한다.
	}
}

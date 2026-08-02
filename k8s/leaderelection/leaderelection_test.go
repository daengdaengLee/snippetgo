package leaderelection

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"k8s.io/client-go/kubernetes/fake"
)

func TestApplyDefaults(t *testing.T) {
	t.Run("빈 타이밍 값은 Default* 로 채워진다", func(t *testing.T) {
		got := Config{}.applyDefaults()
		if got.LeaseDuration != DefaultLeaseDuration {
			t.Errorf("LeaseDuration = %v, want %v", got.LeaseDuration, DefaultLeaseDuration)
		}
		if got.RenewDeadline != DefaultRenewDeadline {
			t.Errorf("RenewDeadline = %v, want %v", got.RenewDeadline, DefaultRenewDeadline)
		}
		if got.RetryPeriod != DefaultRetryPeriod {
			t.Errorf("RetryPeriod = %v, want %v", got.RetryPeriod, DefaultRetryPeriod)
		}
	})

	t.Run("지정한 타이밍 값은 유지된다", func(t *testing.T) {
		in := Config{
			LeaseDuration: 30 * time.Second,
			RenewDeadline: 20 * time.Second,
			RetryPeriod:   5 * time.Second,
		}
		got := in.applyDefaults()
		if got.LeaseDuration != in.LeaseDuration ||
			got.RenewDeadline != in.RenewDeadline ||
			got.RetryPeriod != in.RetryPeriod {
			t.Errorf("타이밍 값이 덮어써짐: got %+v", got)
		}
	})
}

func TestValidate(t *testing.T) {
	base := Config{
		Namespace: "ns",
		LeaseName: "lease",
		Identity:  "pod-a",
	}.applyDefaults()

	t.Run("정상 설정은 통과한다", func(t *testing.T) {
		if err := base.validate(); err != nil {
			t.Fatalf("예상치 못한 에러: %v", err)
		}
	})

	t.Run("필수 값이 비면 ErrInvalidConfig", func(t *testing.T) {
		for name, mutate := range map[string]func(*Config){
			"namespace 없음": func(c *Config) { c.Namespace = "" },
			"leaseName 없음": func(c *Config) { c.LeaseName = "" },
			"identity 없음":  func(c *Config) { c.Identity = "" },
		} {
			c := base
			mutate(&c)
			if err := c.validate(); !errors.Is(err, ErrInvalidConfig) {
				t.Errorf("%s: err = %v, want ErrInvalidConfig", name, err)
			}
		}
	})

	// 타이밍 관계식은 validate 가 아니라 client-go NewLeaderElector 가 검사한다.
	// (그 위임은 TestRunRejectsBadTiming 에서 확인)
}

// 잘못된 타이밍은 validate 가 아니라 NewLeaderElector 에서 걸러져야 한다. Run 이 그 에러를
// 그대로 반환하는지 확인한다. 잘못된 타이밍은 선출 루프 진입 전에 실패하므로 블록하지 않는다.
func TestRunRejectsBadTiming(t *testing.T) {
	cfg := Config{
		Namespace:     "ns",
		LeaseName:     "lease",
		Identity:      "pod-a",
		LeaseDuration: 1 * time.Second, // LeaseDuration <= RenewDeadline -> NewLeaderElector 에러
		RenewDeadline: 2 * time.Second,
	}
	err := Run(context.Background(), fake.NewClientset(), cfg)
	if err == nil {
		t.Fatal("잘못된 타이밍인데 에러가 없다")
	}
	if errors.Is(err, ErrInvalidConfig) {
		t.Errorf("err = %v, want NewLeaderElector 의 타이밍 에러(ErrInvalidConfig 아님)", err)
	}
}

// 이미 취소된 ctx 로 부르면 Run/RunUntilCancelled 은 패닉 없이 nil 을 반환해야 한다
// (ctx 로 멈춘 정상 종료 경로). fake clientset 으로 client-go 실제 경로를 태운다.
func TestRunReturnsOnCancelledContext(t *testing.T) {
	cfg := Config{Namespace: "ns", LeaseName: "lease", Identity: "pod-a"}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 시작 전에 취소

	if err := Run(ctx, fake.NewClientset(), cfg); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
	if err := RunUntilCancelled(ctx, fake.NewClientset(), cfg); err != nil {
		t.Errorf("RunUntilCancelled = %v, want nil", err)
	}
}

// 설정 오류는 ErrLostLease 가 아니므로 RunUntilCancelled 이 재시도하지 않고 그대로 반환해야 한다.
func TestRunUntilCancelledDoesNotRetryConfigError(t *testing.T) {
	cfg := Config{LeaseName: "lease", Identity: "pod-a"} // Namespace 누락
	err := RunUntilCancelled(context.Background(), fake.NewClientset(), cfg)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("RunUntilCancelled = %v, want ErrInvalidConfig", err)
	}
}

// client-go 계약: OnStoppedLeading 은 Run 종료 시 항상 호출된다(리더가 된 적 없어도).
// 미리 취소한 ctx 는 acquire 진입 전 종료라 리더가 되지 못하지만(OnStartedLeading 0 회),
// defer 로 OnStoppedLeading 은 1 회 불린다. 래퍼가 이를 그대로 전달함을 확인한다.
func TestOnStoppedLeadingAlwaysCalledOnExit(t *testing.T) {
	var started, stopped atomic.Int32
	cfg := Config{
		Namespace:        "ns",
		LeaseName:        "lease",
		Identity:         "pod-a",
		OnStartedLeading: func(context.Context) { started.Add(1) },
		OnStoppedLeading: func() { stopped.Add(1) },
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // acquire 진입 전 종료 -> 리더가 되지 못함

	if err := Run(ctx, fake.NewClientset(), cfg); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if got := started.Load(); got != 0 {
		t.Errorf("OnStartedLeading 호출 %d 회, want 0 (리더가 되지 못함)", got)
	}
	if got := stopped.Load(); got != 1 {
		t.Errorf("OnStoppedLeading 호출 %d 회, want 1 (client-go 는 종료 시 항상 호출)", got)
	}
}

// 여기부터는 선출 루프를 실제로 돌리는 테스트다. fake clientset 이 Lease 를
// get/create/update 하는 경로를 그대로 태우므로 클러스터 없이도 획득/상실/재경쟁을
// 재현할 수 있다. 헬퍼는 helper_test.go 참고.

// 리더 획득 성공 경로: OnStartedLeading 이 정확히 한 번 불리고, ctx 취소로 끝나면 nil 이다.
func TestRunAcquiresLeadership(t *testing.T) {
	var started atomic.Int32
	cfg := fastConfig()
	cfg.OnStartedLeading = func(ctx context.Context) {
		started.Add(1)
		<-ctx.Done()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, fake.NewClientset(), cfg) }()

	awaitCount(t, &started, 1, 5*time.Second, "OnStartedLeading")
	cancel()

	if err := awaitErr(t, errCh, 5*time.Second, "Run"); err != nil {
		t.Errorf("Run = %v, want nil (ctx 취소는 정상 종료)", err)
	}
	if got := started.Load(); got != 1 {
		t.Errorf("OnStartedLeading 호출 %d 회, want 1", got)
	}
}

// OnNewLeader 는 관찰된 리더가 바뀔 때마다 불린다 - 자기 자신이 리더가 된 경우도 포함이라
// 참가자가 하나여도 확인할 수 있다. 세 콜백 중 이것만 pass-through 검증이 없었다.
func TestOnNewLeaderReceivesLeaderIdentity(t *testing.T) {
	// 버퍼 1 + non-blocking send. client-go 는 OnNewLeader 를 별도 goroutine 으로 띄우므로
	// (maybeReportTransition 의 go 호출) 콜백이 블록해도 선출 루프는 멈추지 않는다. 여기서
	// 막으려는 건 테스트가 값을 하나 받고 끝난 뒤 두 번째 송신이 영영 블록해 goroutine 이
	// 남는 것이다.
	seen := make(chan string, 1)
	cfg := fastConfig()
	cfg.OnStartedLeading = func(ctx context.Context) { <-ctx.Done() }
	cfg.OnNewLeader = func(identity string) {
		select {
		case seen <- identity:
		default:
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, fake.NewClientset(), cfg) }()

	select {
	case got := <-seen:
		if got != cfg.Identity {
			t.Errorf("OnNewLeader(%q), want %q", got, cfg.Identity)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("OnNewLeader 가 호출되지 않았다")
	}

	cancel()
	if err := awaitErr(t, errCh, 5*time.Second, "Run"); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
}

// 함정 3: ReleaseOnCancel 로 ctx 취소 시 Lease 를 즉시 반납한다. 반납되면 client-go 가
// holderIdentity 를 빈 문자열로 덮어쓰므로, 다음 후보가 LeaseDuration 만료를 기다리지 않고
// 바로 이어받는다(failover 가 LeaseDuration 이 아니라 수백 ms 로 끝나는 이유).
//
// 여기서는 newRenewBlocker 가 아니라 fake clientset 을 직접 쓴다 - 갱신을 막을 필요가 없고,
// Run 과 조회가 같은 인스턴스를 쓴다는 게 지역변수 하나로 자명해진다.
func TestRunReleasesLeaseOnCancel(t *testing.T) {
	client := fake.NewClientset()

	var started atomic.Int32
	cfg := fastConfig()
	cfg.OnStartedLeading = func(ctx context.Context) {
		started.Add(1)
		<-ctx.Done()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, client, cfg) }()

	awaitCount(t, &started, 1, 5*time.Second, "OnStartedLeading")
	if got := leaseHolder(t, client, cfg.Namespace, cfg.LeaseName); got != cfg.Identity {
		t.Fatalf("취소 전 holderIdentity = %q, want %q", got, cfg.Identity)
	}

	cancel()
	if err := awaitErr(t, errCh, 5*time.Second, "Run"); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}

	// client-go 는 반납을 renew 끝에서 처리하고 그 뒤에 elector.Run 이 반환하므로,
	// Run 이 돌아온 시점에는 반납이 이미 반영돼 있다.
	if got := leaseHolder(t, client, cfg.Namespace, cfg.LeaseName); got != "" {
		t.Errorf("취소 후 holderIdentity = %q, want 빈 문자열 (ReleaseOnCancel 이 반납해야 한다)", got)
	}
}

// 리더가 된 뒤 갱신이 실패하면 ctx 는 살아 있으므로 ErrLostLease 여야 한다.
// nil(정상 종료) 과 구분되는 이 신호가 exit 모드의 근거다.
func TestRunReturnsErrLostLeaseOnRenewFailure(t *testing.T) {
	blocker := newRenewBlocker()

	var started atomic.Int32
	cfg := fastConfig()
	cfg.OnStartedLeading = func(ctx context.Context) {
		started.Add(1)
		<-ctx.Done()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- Run(ctx, blocker.client, cfg) }()

	awaitCount(t, &started, 1, 5*time.Second, "OnStartedLeading")
	blocker.Block() // 이제부터 갱신 실패 -> 리더직 비자발적 상실

	if err := awaitErr(t, errCh, 10*time.Second, "Run"); !errors.Is(err, ErrLostLease) {
		t.Errorf("Run = %v, want ErrLostLease", err)
	}
}

// ctx 가 "취소" 가 아닌 사유로 끝나면 nil(정상 종료) 이 아니라 그 ctx 에러를 그대로 알린다.
// 둘을 뭉뚱그리면 데드라인 설정 실수가 조용히 정상 종료로 묻힌다.
func TestRunReturnsContextErrorOnDeadline(t *testing.T) {
	cfg := fastConfig()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := Run(ctx, fake.NewClientset(), cfg); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Run = %v, want context.DeadlineExceeded", err)
	}

	// RunUntilCancelled 도 ErrLostLease 가 아닌 에러는 재시도하지 않고 그대로 돌려준다.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := RunUntilCancelled(ctx2, fake.NewClientset(), cfg); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("RunUntilCancelled = %v, want context.DeadlineExceeded", err)
	}
}

// RunUntilCancelled 의 핵심 계약: 리더직을 잃어도 같은 프로세스가 다시 후보로 참여해
// 재획득한다(OnStartedLeading 이 두 번째로 불린다).
func TestRunUntilCancelledRejoinsAfterLostLease(t *testing.T) {
	blocker := newRenewBlocker()

	var started, stopped atomic.Int32
	cfg := fastConfig()
	cfg.OnStartedLeading = func(ctx context.Context) {
		started.Add(1)
		<-ctx.Done()
	}
	// 상실 확정을 붙잡는 신호. client-go 는 이 콜백을 LeaderElector.Run 의 defer 로
	// 부르므로, 불렸다는 건 renew 가 이미 포기했다는 뜻이다.
	cfg.OnStoppedLeading = func() { stopped.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- RunUntilCancelled(ctx, blocker.client, cfg) }()

	awaitCount(t, &started, 1, 5*time.Second, "최초 OnStartedLeading")
	blocker.Block() // 상실 유도
	// sleep 으로 "이쯤이면 됐겠지" 하고 넘기면 느린 머신에서 아직 리더인 채로 Unblock 해
	// 재획득이 일어나지 않는다. 상실이 확정된 시점을 신호로 기다린다.
	awaitCount(t, &stopped, 1, 5*time.Second, "OnStoppedLeading(상실 확정)")
	blocker.Unblock() // 재획득 허용

	awaitCount(t, &started, 2, 10*time.Second, "재경쟁 후 OnStartedLeading")

	cancel()
	if err := awaitErr(t, errCh, 5*time.Second, "RunUntilCancelled"); err != nil {
		t.Errorf("RunUntilCancelled = %v, want nil", err)
	}
}

// WaitForLeaderWork 를 켜면 Run 이 리더 작업 종료를 기다리므로, rejoin 으로 재획득해도
// 같은 프로세스 안에서 연달아 실행되는 두 OnStartedLeading 의 동시 실행이 1 을 넘지 않는다.
// 이 테스트가 고정하는 건 그 인프로세스 겹침 하나뿐이다 - 다른 프로세스와의 겹침이나
// OnStoppedLeading 과의 겹침은 이 옵션이 막아 주지 않는다(README 함정 5).
//
// 반대 방향(기본값 false 에서 겹침이 "발생함") 은 단언하지 않는다 - 재획득 속도에 따라
// 겹치지 않을 수도 있는 타이밍 의존 현상이라 flaky 한 테스트가 된다.
func TestRunUntilCancelledWaitForLeaderWorkPreventsOverlap(t *testing.T) {
	blocker := newRenewBlocker()

	var started, stopped, concurrent, maxConcurrent atomic.Int32
	cfg := fastConfig()
	cfg.WaitForLeaderWork = true
	cfg.OnStartedLeading = func(ctx context.Context) {
		cur := concurrent.Add(1)
		for {
			observed := maxConcurrent.Load()
			if cur <= observed || maxConcurrent.CompareAndSwap(observed, cur) {
				break
			}
		}
		// started 는 겹침 기록이 끝난 뒤에 올린다. 메인 goroutine 이 started >= 2 를 게이트로
		// maxConcurrent 를 읽으므로, 순서가 반대면 두 번째 콜백이 started 만 올리고 아직 CAS 를
		// 못 한 창에서 읽혀 깨진 구현이 maxConcurrent == 1 로 조용히 통과할 수 있다.
		started.Add(1)
		<-ctx.Done()
		// 리더 작업이 정리에 시간을 쓰는 현실적인 상황. 대기가 없으면 이 사이에
		// 다음 리더 작업이 시작돼 동시 실행이 2 가 된다.
		//
		// 이 sleep 은 판정을 흔들지 않는다 - 느린 머신에서는 겹침 창이 넓어지므로
		// 거짓 통과가 아니라 거짓 실패 방향이다(깨진 구현이 더 잘 잡힌다).
		time.Sleep(400 * time.Millisecond)
		concurrent.Add(-1)
	}
	cfg.OnStoppedLeading = func() { stopped.Add(1) }

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	errCh := make(chan error, 1)
	go func() { errCh <- RunUntilCancelled(ctx, blocker.client, cfg) }()

	awaitCount(t, &started, 1, 5*time.Second, "최초 OnStartedLeading")
	blocker.Block()
	// 위 테스트와 같은 이유로 sleep 대신 상실 확정 신호를 기다린다. 여기서는 대기가
	// 끝나야(= Run 이 반환해야) 다음 판이 열리므로, 일찍 Unblock 해도 겹침 판정이
	// 흐려지지 않는다 - 오히려 재획득을 앞당겨 깨진 구현을 더 잘 잡는다.
	awaitCount(t, &stopped, 1, 5*time.Second, "OnStoppedLeading(상실 확정)")
	blocker.Unblock()

	awaitCount(t, &started, 2, 15*time.Second, "재경쟁 후 OnStartedLeading")

	if got := maxConcurrent.Load(); got != 1 {
		t.Errorf("최대 동시 실행 = %d, want 1 (WaitForLeaderWork 가 같은 프로세스 안 겹침을 막아야 한다)", got)
	}

	cancel()
	if err := awaitErr(t, errCh, 10*time.Second, "RunUntilCancelled"); err != nil {
		t.Errorf("RunUntilCancelled = %v, want nil", err)
	}
}

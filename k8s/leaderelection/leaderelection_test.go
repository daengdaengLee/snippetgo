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
	err := Run(context.Background(), fake.NewSimpleClientset(), cfg)
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

	if err := Run(ctx, fake.NewSimpleClientset(), cfg); err != nil {
		t.Errorf("Run = %v, want nil", err)
	}
	if err := RunUntilCancelled(ctx, fake.NewSimpleClientset(), cfg); err != nil {
		t.Errorf("RunUntilCancelled = %v, want nil", err)
	}
}

// 설정 오류는 ErrLostLease 가 아니므로 RunUntilCancelled 이 재시도하지 않고 그대로 반환해야 한다.
func TestRunUntilCancelledDoesNotRetryConfigError(t *testing.T) {
	cfg := Config{LeaseName: "lease", Identity: "pod-a"} // Namespace 누락
	err := RunUntilCancelled(context.Background(), fake.NewSimpleClientset(), cfg)
	if !errors.Is(err, ErrInvalidConfig) {
		t.Errorf("RunUntilCancelled = %v, want ErrInvalidConfig", err)
	}
}

// 리더가 된 적 없는 경우(취소된 ctx -> acquire 진입 전 종료) OnStoppedLeading 은 호출되면 안 된다.
// client-go 는 defer 로 무조건 부르지만 래퍼가 startedLeading 가드로 막는다.
func TestOnStoppedLeadingGuardedForFollower(t *testing.T) {
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

	if err := Run(ctx, fake.NewSimpleClientset(), cfg); err != nil {
		t.Fatalf("Run = %v, want nil", err)
	}
	if got := started.Load(); got != 0 {
		t.Errorf("OnStartedLeading 호출 %d 회, want 0", got)
	}
	if got := stopped.Load(); got != 0 {
		t.Errorf("OnStoppedLeading 호출 %d 회, want 0 (팔로워는 정리 콜백을 받으면 안 됨)", got)
	}
}

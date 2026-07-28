package leaderelection

import (
	"context"
	"errors"
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

	t.Run("LeaseDuration <= RenewDeadline 이면 에러", func(t *testing.T) {
		c := base
		c.RenewDeadline = c.LeaseDuration
		if err := c.validate(); err == nil {
			t.Error("LeaseDuration <= RenewDeadline 인데 에러가 없다")
		}
	})

	t.Run("RenewDeadline <= RetryPeriod*JitterFactor 면 에러", func(t *testing.T) {
		// RetryPeriod < RenewDeadline 은 만족하지만 JitterFactor(1.2) 를 곱하면
		// 1.2*9s=10.8s >= 10s 라 client-go 와 마찬가지로 거부되어야 한다.
		c := Config{
			Namespace:     "ns",
			LeaseName:     "lease",
			Identity:      "pod-a",
			LeaseDuration: 15 * time.Second,
			RenewDeadline: 10 * time.Second,
			RetryPeriod:   9 * time.Second,
		}
		if err := c.validate(); err == nil {
			t.Error("RenewDeadline <= RetryPeriod*JitterFactor 인데 에러가 없다")
		}
	})
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

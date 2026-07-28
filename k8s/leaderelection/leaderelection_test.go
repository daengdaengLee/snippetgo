package leaderelection

import (
	"errors"
	"testing"
	"time"
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

	t.Run("타이밍 관계식이 깨지면 에러", func(t *testing.T) {
		c := base
		c.RenewDeadline = c.LeaseDuration // RenewDeadline >= LeaseDuration
		if err := c.validate(); err == nil {
			t.Error("RenewDeadline >= LeaseDuration 인데 에러가 없다")
		}
	})
}

package leaderelection

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

// fastConfig 는 선출 루프를 실제로 돌리는 테스트용 설정이다. 기본값(15s/10s/2s) 은
// 테스트에 너무 느려서, 관계식(RetryPeriod * 1.2 < RenewDeadline < LeaseDuration) 을
// 지키는 선에서 최대한 짧게 잡는다.
func fastConfig() Config {
	return Config{
		Namespace:     "ns",
		LeaseName:     "lease",
		Identity:      "pod-a",
		LeaseDuration: 300 * time.Millisecond,
		RenewDeadline: 200 * time.Millisecond,
		RetryPeriod:   50 * time.Millisecond,
	}
}

// renewBlocker 는 Lease update 를 마음대로 실패시킬 수 있는 fake clientset 이다.
// 갱신을 막으면 client-go 가 임대를 놓쳐 리더직 비자발적 상실이 재현된다.
type renewBlocker struct {
	client kubernetes.Interface
	fail   atomic.Bool
}

// newRenewBlocker 는 update 를 가로채는 reactor 를 단 fake clientset 을 만든다.
// 기본 상태는 통과(정상 갱신) 이고, Block 을 부르면 그때부터 update 가 실패한다.
func newRenewBlocker() *renewBlocker {
	b := &renewBlocker{}
	c := fake.NewClientset()
	c.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
		if b.fail.Load() {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Group: "coordination.k8s.io", Resource: "leases"},
				"lease",
				errors.New("테스트용 갱신 실패"))
		}
		return false, nil, nil // 기본 트래커로 넘긴다
	})
	b.client = c
	return b
}

// Block 은 이후의 Lease update 를 모두 실패시킨다(리더직 상실 유도).
func (b *renewBlocker) Block() { b.fail.Store(true) }

// Unblock 은 다시 정상 갱신을 허용한다(재획득 허용).
func (b *renewBlocker) Unblock() { b.fail.Store(false) }

// awaitCount 는 counter 가 want 이상이 될 때까지 기다린다. 선출 루프는 타이머 기반이라
// 폴링으로 기다리고, 제한 시간을 넘기면 테스트를 실패시킨다.
func awaitCount(t *testing.T, counter *atomic.Int32, want int32, timeout time.Duration, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if counter.Load() >= want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("%s: %d 회에 도달하지 못했다 (현재 %d 회)", what, want, counter.Load())
}

// awaitErr 는 Run 계열 호출의 반환을 기다려 에러를 돌려준다.
func awaitErr(t *testing.T, errCh <-chan error, timeout time.Duration, what string) error {
	t.Helper()
	select {
	case err := <-errCh:
		return err
	case <-time.After(timeout):
		t.Fatalf("%s: 반환하지 않았다", what)
		return nil
	}
}

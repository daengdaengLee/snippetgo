// Command main 은 leaderelection.Run 을 손으로 확인해 보기 위한 데모다.
//
// 인클러스터(파드 안)면 ServiceAccount 를 자동으로 쓰고, 클러스터 밖이면
// -kubeconfig/KUBECONFIG/~/.kube/config 로 폴백한다. 같은 이미지를 replica 여러 개로
// 띄우면 그중 하나만 "started leading" 로그를 남긴다.
//
// 사용법은 k8s/leaderelection/README.md 의 "직접 확인해보기" 를 참고.
package main

import (
	"cmp"
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/daengdaengLee/snippetgo/k8s/leaderelection"
)

func main() {
	kubeconfig := flag.String("kubeconfig", "", "클러스터 밖에서 실행할 때 쓸 kubeconfig 경로 (비우면 KUBECONFIG/~/.kube/config)")
	flag.Parse()

	// identity 는 그룹 안에서 유일해야 한다. 파드 안이면 Downward API 로 주입한
	// POD_NAME 을 쓰고, 없으면 호스트네임으로 대체한다.
	hostname, _ := os.Hostname()
	identity := cmp.Or(os.Getenv("POD_NAME"), hostname)
	namespace := cmp.Or(os.Getenv("POD_NAMESPACE"), "default")
	leaseName := cmp.Or(os.Getenv("LEASE_NAME"), "snippetgo-leaderelection")

	client, err := newClient(*kubeconfig)
	if err != nil {
		log.Fatalf("kubernetes client 생성 실패: %v", err)
	}

	// SIGTERM(파드 종료 신호)/SIGINT 를 받으면 ctx 를 취소해 Lease 를 반납한다.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	log.Printf("리더 선출 시작: identity=%s namespace=%s lease=%s", identity, namespace, leaseName)

	cfg := leaderelection.Config{
		Namespace: namespace,
		LeaseName: leaseName,
		Identity:  identity,
		OnStartedLeading: func(ctx context.Context) {
			log.Printf("[%s] started leading - 여기서 싱글톤 작업을 돌린다", identity)
			runLeaderWork(ctx, identity)
		},
		OnStoppedLeading: func() {
			log.Printf("[%s] stopped leading - 리더 작업을 정리한다", identity)
		},
		OnNewLeader: func(leader string) {
			if leader == identity {
				return // 자기 자신이 리더가 된 경우는 OnStartedLeading 에서 이미 로그를 남긴다
			}
			log.Printf("[%s] new leader: %s (나는 대기)", identity, leader)
		},
	}

	if err := leaderelection.Run(ctx, client, cfg); err != nil {
		log.Fatalf("리더 선출 종료(에러): %v", err)
	}
	log.Printf("[%s] 종료", identity)
}

// runLeaderWork 는 리더만 도는 예시 작업이다. ctx 가 취소되면(리더직 상실/종료) 멈춘다.
func runLeaderWork(ctx context.Context, identity string) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			log.Printf("[%s] 리더 작업 수행 중...", identity)
		}
	}
}

// newClient 는 인클러스터 설정을 먼저 시도하고, 실패하면 kubeconfig 로 폴백한다.
func newClient(kubeconfig string) (kubernetes.Interface, error) {
	config, err := rest.InClusterConfig()
	if err != nil {
		// 클러스터 밖: -kubeconfig 플래그 -> KUBECONFIG -> ~/.kube/config 순으로 로드한다.
		loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
		if kubeconfig != "" {
			loadingRules.ExplicitPath = kubeconfig
		}
		config, err = clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
			loadingRules, &clientcmd.ConfigOverrides{}).ClientConfig()
		if err != nil {
			return nil, err
		}
	}
	return kubernetes.NewForConfig(config)
}

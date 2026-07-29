// Command main 은 leaderelection 패키지를 손으로 확인해 보기 위한 데모다.
//
// 인클러스터(파드 안)면 ServiceAccount 를 자동으로 쓰고, 클러스터 밖이면
// -kubeconfig/KUBECONFIG/~/.kube/config 로 폴백한다. 같은 이미지를 replica 여러 개로
// 띄우면 그중 하나만 "started leading" 로그를 남긴다.
//
// -mode 로 리더직 상실 처리 방식을 고른다.
//   - exit(기본):  상실 시 프로세스를 비정상 종료 -> Kubernetes 가 Pod 재시작
//   - rejoin:      상실해도 같은 프로세스가 후보로 재참여
//
// 사용법은 k8s/leaderelection/README.md 의 "직접 확인해보기" 를 참고.
package main

import (
	"cmp"
	"context"
	"errors"
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
	mode := flag.String("mode", cmp.Or(os.Getenv("LE_MODE"), "exit"), "리더직 상실 처리: exit | rejoin")
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

	log.Printf("리더 선출 시작: identity=%s namespace=%s lease=%s mode=%s", identity, namespace, leaseName, *mode)

	cfg := leaderelection.Config{
		Namespace: namespace,
		LeaseName: leaseName,
		Identity:  identity,
		OnStartedLeading: func(ctx context.Context) {
			log.Printf("[%s] started leading - 여기서 싱글톤 작업을 돌린다", identity)
			runLeaderWork(ctx, identity)
		},
		OnStoppedLeading: func() {
			// client-go 는 리더가 된 적 없는 팔로워에게도 이 콜백을 준다(README 함정 7).
			// 그래서 "리더였다" 를 단정하지 않는다. 실제 정리는 멱등이어야 한다.
			log.Printf("[%s] OnStoppedLeading 호출됨 (리더였다면 여기서 정리)", identity)
		},
		OnNewLeader: func(leader string) {
			if leader == identity {
				return // 자기 자신이 리더가 된 경우는 OnStartedLeading 에서 이미 로그를 남긴다
			}
			log.Printf("[%s] new leader: %s (나는 대기)", identity, leader)
		},
	}

	switch *mode {
	case "exit":
		// 리더직 상실을 프로세스 종료로 승격한다. Kubernetes 가 Pod 를 재시작해
		// 깨끗한 상태로 다시 경쟁에 합류시킨다(controller-runtime 표준 동작).
		err := leaderelection.Run(ctx, client, cfg)
		if errors.Is(err, leaderelection.ErrLostLease) {
			log.Fatalf("[%s] 리더직 상실 - 재시작에 위임한다: %v", identity, err)
		}
		if err != nil {
			log.Fatalf("[%s] 리더 선출 종료(에러): %v", identity, err)
		}
	case "rejoin":
		// 리더직을 잃어도 같은 프로세스가 후보로 재참여한다.
		if err := leaderelection.RunUntilCancelled(ctx, client, cfg); err != nil {
			log.Fatalf("[%s] 리더 선출 종료(에러): %v", identity, err)
		}
	default:
		log.Fatalf("알 수 없는 -mode=%q (exit | rejoin)", *mode)
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

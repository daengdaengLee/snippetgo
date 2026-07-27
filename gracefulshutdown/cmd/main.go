// Command main 은 gracefulshutdown.Serve 를 손으로 확인해 보기 위한 데모 서버다.
//
// 사용법은 gracefulshutdown/README.md 의 "직접 확인해보기" 를 참고.
package main

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/daengdaengLee/snippetgo/gracefulshutdown"
)

func main() {
	addr := flag.String("addr", "127.0.0.1:8080", "listen address")
	drain := flag.Duration("drain", 3*time.Second, "how long to keep serving after the shutdown signal before closing the listener")
	timeout := flag.Duration("timeout", 10*time.Second, "max time to wait for in-flight requests")
	flag.Parse()

	// 시그널을 context 취소로 바꾸는 지점은 여기 하나뿐이다.
	// 라이브러리는 context 만 알면 되므로 테스트하기 쉬워진다.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 첫 시그널을 받는 즉시 핸들러를 해제한다. SIGINT/SIGTERM 은 runtime sigtable 에서
	// _SigKill 이라, 등록된 채널이 하나도 남지 않으면 두 번째 시그널이 프로세스를 바로 죽인다.
	// 긴 DrainDelay 의 탈출구다. 훅 안에서 하면 Serve 가 깨어날 때까지 늦어진다.
	//
	// 남는 한계 둘은 README 의 함정 9 를 참고. 요약하면 (1) 시그널 도착과 stop 실행 사이
	// goroutine 두 번의 창, (2) SIGINT 을 SIG_IGN 으로 물려받은 프로세스에서는 무효.
	context.AfterFunc(ctx, stop)

	var ready atomic.Bool
	ready.Store(true)

	srv := &http.Server{
		Handler:           newMux(&ready),
		ReadHeaderTimeout: 5 * time.Second,
	}

	// 리스너를 호출자가 직접 만든다. 어디에 바인딩됐는지 알 수 있고 TLS 로 감싸기도 쉽다.
	ln, err := net.Listen("tcp", *addr)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	log.Printf("listening on http://%s (drain=%v, timeout=%v)", ln.Addr(), *drain, *timeout)

	err = gracefulshutdown.Serve(ctx, srv, ln, gracefulshutdown.Config{
		ShutdownTimeout: *timeout,
		DrainDelay:      *drain,
		OnShutdownStart: func() {
			ready.Store(false)
			log.Println("readiness turned off; press Ctrl+C again to exit immediately")
		},
	})

	switch {
	case err == nil:
		log.Println("shutdown complete")
	case errors.Is(err, gracefulshutdown.ErrShutdownTimeout):
		log.Printf("forced shutdown; some requests did not finish in time: %v", err)
		os.Exit(1)
	case errors.Is(err, gracefulshutdown.ErrListenerClose):
		// 요청은 모두 정상 처리됐다. 배포 실패로 볼 이유가 없다.
		log.Printf("shutdown complete, but closing the listener failed: %v", err)
	default:
		log.Printf("shutdown: %v", err)
		os.Exit(1)
	}
}

func newMux(ready *atomic.Bool) *http.ServeMux {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("hello\n"))
	})

	// /slow 는 Shutdown 이 r.Context() 를 취소하지 않는다는 사실을 눈으로 보여 준다.
	// graceful 종료 중에는 timer 가 이겨서 정상 응답이 나가고,
	// 유예를 넘겨 hard stop 이 걸리면 그때서야 ctx 가 이긴다.
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		d, err := time.ParseDuration(cmp.Or(r.URL.Query().Get("d"), "5s"))
		if err != nil {
			http.Error(w, "invalid d: "+err.Error(), http.StatusBadRequest)
			return
		}

		log.Printf("/slow started (d=%v)", d)
		timer := time.NewTimer(d)
		defer timer.Stop()

		select {
		case <-timer.C:
			log.Println("/slow: timer fired -> responding normally")
			_, _ = w.Write([]byte("slow done\n"))
		case <-r.Context().Done():
			log.Printf("/slow: request canceled -> %v", r.Context().Err())
		}
	})

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	})

	// readiness 는 애플리케이션이 소유하는 상태다.
	// 라이브러리는 OnShutdownStart 훅으로 "지금 내려라"만 알려 준다.
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		if !ready.Load() {
			http.Error(w, "draining", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte("ready\n"))
	})

	return mux
}

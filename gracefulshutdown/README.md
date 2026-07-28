# HTTP Server Graceful Shutdown

`net/http` 서버를 **context 로 제어해** 안전하게 종료하는 코드 스니펫.

## 무엇을 해결하나

배포 중에 502 가 나는 이유는 대부분 `srv.Shutdown()` 을 몰라서가 아니라 그 주변의 함정들 때문이다.

```
SIGTERM --+-----------------+------------------+--------------+-------->
          |                 |                  |              |
       readiness         DrainDelay         리스너 닫고     유예 초과 시
        503 로            (LB 가 나를        in-flight       hard stop +
        전환              빼갈 시간)          대기            강제 종료
          |                 |                  |              |
새 연결:  받음              받음               거부            거부
in-flight: 정상 처리        정상 처리          정상 처리       중단
r.Context(): 살아있음       살아있음           살아있음        취소됨
                                        (hijack 된 커넥션은 예외, 함정 7)
```

이 순서가 어긋나면 이런 일이 생긴다.

- readiness 를 안 내리고 리스너부터 닫으면, 로드밸런서가 아직 나에게 트래픽을 보내는 동안 연결이 거부된다 -> **502**
- 유예 시간을 안 주면 `Shutdown` 은 **무한히** 기다린다 -> Kubernetes 가 `terminationGracePeriodSeconds` 후 SIGKILL
- 이미 취소된 context 로 타임아웃을 파생시키면 graceful 이 통째로 무력화된다 -> **코드는 멀쩡해 보이는데** in-flight 요청이 끊긴다

## 빠른 시작

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

// 첫 시그널이 오는 즉시 핸들러를 해제해 두 번째 Ctrl+C 로 빠져나갈 수 있게 한다. 함정 9.
context.AfterFunc(ctx, stop)

ln, err := net.Listen("tcp", "127.0.0.1:8080")
if err != nil {
    log.Fatal(err)
}

srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}

err = gracefulshutdown.Serve(ctx, srv, ln, gracefulshutdown.Config{
    ShutdownTimeout: 10 * time.Second,
    DrainDelay:      3 * time.Second,
    OnShutdownStart: func() { ready.Store(false) },
})
// 리스너 close 에러는 요청을 다 비운 뒤의 일이라 배포 실패가 아니다.
if err != nil && !errors.Is(err, gracefulshutdown.ErrListenerClose) {
    log.Fatalf("shutdown: %v", err)
}
```

시그널을 context 취소로 바꾸는 일은 `main` 에서만 한다. 라이브러리는 context 만 알기 때문에
테스트에서 시그널을 흉내 낼 필요 없이 `cancel()` 한 번으로 종료 전 과정을 재현할 수 있다.

## API

```go
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, cfg Config) error
```

| Config 필드 | 설명 |
| --- | --- |
| `ShutdownTimeout` | in-flight 요청을 기다리는 최대 시간. `0` 이하면 `DefaultShutdownTimeout`(10s) |
| `DrainDelay` | 종료 신호 후 리스너를 닫기까지 기다리는 시간. 이 동안에도 새 요청을 받는다. `srv.Serve` 가 반환하거나 `AbortDrain` 이 닫히면 남은 시간을 건너뛴다 |
| `OnShutdownStart` | 정리를 시작하기 직전 **동기** 호출. ctx 취소든 `srv.Serve` 선행 종료든 정확히 한 번. readiness 를 503 으로 내리는 용도 |
| `AbortDrain` | 닫히면 남은 `DrainDelay` 를 건너뛴다. 시그널을 쓸 수 없는 환경(테스트, 라이브러리 임베딩)을 위한 경로 |

**에러 규약**

| 상황 | 반환값 |
| --- | --- |
| 정상 종료 | `nil` (`http.ErrServerClosed` 는 **종료 신호 이후에 온 것만** 정규화) |
| 유예 초과 | `errors.Is(err, ErrShutdownTimeout)` 과 `errors.Is(err, context.DeadlineExceeded)` 가 **모두** 참 |
| 요청은 다 비웠지만 리스너 close 실패 | `errors.Is(err, ErrListenerClose)`. 배포 자체는 성공이다 |
| 그 밖 | 감싸지 않고 그대로 전달. 신호 전에 온 `http.ErrServerClosed` 도 여기 해당한다 |

**소유권**

- `ln` 은 `Serve` 가 가져간다. `srv.Serve` 가 반환할 때 닫히므로 호출자가 따로 닫지 않는다.
- `srv.BaseContext` 는 `Serve` 가 덮어쓰고 **반환할 때 원래 값으로 되돌린다.**
  기존 값이 있으면 그 위에 체인하므로 호출자가 심어 둔 값은 유지된다.
- hijack 된 커넥션의 `r.Context()` 는 커넥션보다 먼저 죽는다. 함정 7 참고.
- 하나의 `srv` 로 `Serve` 를 동시에 또는 반복해서 호출하면 안 된다.
  `net/http` godoc 이 *"Once Shutdown has been called on a server, it may not be reused"* 라고 못박고 있다.

## 종료 시퀀스

```go
hardStop, stopHard := context.WithCancel(context.WithoutCancel(ctx))   // (1)
srv.BaseContext = func(net.Listener) context.Context { return hardStop }

// ctx 취소 대기 -> OnShutdownStart -> DrainDelay
// drain 은 srv.Serve 가 끝나거나 Config.AbortDrain 이 닫히면 앞당겨진다.

shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), cfg.ShutdownTimeout) // (2)
defer cancel()

stop := context.AfterFunc(shutdownCtx, stopHard)                       // (3)
defer stop()

err := srv.Shutdown(shutdownCtx)
if err == nil && shutdownCtx.Err() != nil {                            // (4)
    err = shutdownCtx.Err()
}
switch {
case errors.Is(err, context.DeadlineExceeded):
    return fmt.Errorf("%w: %w", ErrShutdownTimeout, err)
case err != nil:                                                       // (5)
    return fmt.Errorf("%w: %w", ErrListenerClose, err)
}
```

(1) `context.WithoutCancel` 이 없으면 종료 신호가 오는 순간 모든 `r.Context()` 가 죽는다. 값(trace, logger)은 그대로 남는다.
(2) 여기서도 마찬가지다. 취소된 부모로 `WithTimeout` 하면 파생 context 가 즉시 취소되어 `Shutdown` 이 곧바로 반환한다.
(3) `Shutdown` 은 리스너가 반환할 때까지 ctx 를 보지 않는다. 그래서 hard stop 만 `AfterFunc` 으로 떼어 내 마감 시각에 정확히 걸리게 한다. 반환 시각까지 보장되지는 않는다 - 함정 3.
(4) `Shutdown` 의 폴링 루프는 `closeIdleConns()` 를 `ctx.Done()` 보다 먼저 검사한다. hard stop 이 in-flight 를 끊어 낸 덕분에 quiescent 가 되면 마감이 지났는데도 `nil` 이 돌아오므로, 요청을 강제로 끊고도 성공으로 보고하지 않도록 마감을 직접 본다.
(5) 여기 남는 `err` 는 리스너를 닫으면서 난 것뿐이다. 요청은 다 비웠으므로 배포 실패가 아니다.

남은 커넥션을 실제로 끊는 것은 `Serve` 의 정리 defer 다. 이 defer 는 `stopHard()` -> `srv.Close()` ->
`srv.Serve` 반환 대기 -> `BaseContext` 복원 순서로 도는데, 순서 하나하나가 의미를 갖는다.
훅이 panic 해도 서버가 열린 채로 남지 않는 것도 여기서 보장된다.

> **hard stop 의 한계**: hard stop 은 마감 시각에 `AfterFunc` 으로 전달되고, 남은 커넥션은
> `Serve` 가 반환하기 직전 정리 defer 의 `srv.Close()` 가 끊는다. 그래서 핸들러가 만든 응답이
> 클라이언트까지 도달하는 일은 보통 없다. 이 신호의 값은 응답이 아니라 **정리**에 있다 -
> `defer` 가 돌고, 트랜잭션이 롤백되고, `r.Context()` 로 파생된 DB 쿼리와 goroutine 이 멈춘다.

## 직접 확인해보기

```bash
go build -o /tmp/gsdemo ./gracefulshutdown/cmd && /tmp/gsdemo -drain 3s -timeout 10s
```

`go run` 으로 띄우면 툴체인이 자식의 시그널 종료를 `signal: interrupt` + exit 1 로 감싸므로
종료 코드를 확인할 수 없다. 빌드한 바이너리를 직접 실행한다.

| 경로 | 용도 |
| --- | --- |
| `GET /` | 즉시 200 |
| `GET /slow?d=5s` | `d` 만큼 걸리는 요청. timer 와 `r.Context()` 중 어느 쪽이 이겼는지 로그로 보여 준다 |
| `GET /healthz` | liveness. 항상 200 |
| `GET /readyz` | readiness. 종료가 시작되면 503 |

### 1. 정상 graceful 종료

```bash
# 터미널 1
/tmp/gsdemo -drain 3s -timeout 10s
# 터미널 2 - 5초 걸리는 요청을 띄워 둔다
curl -v "http://127.0.0.1:8080/slow?d=5s"
# 터미널 1 에서 Ctrl+C. 곧바로 터미널 3 에서:
curl -i http://127.0.0.1:8080/readyz   # 503  <- LB 가 여기서 나를 뺀다
curl -i http://127.0.0.1:8080/         # 200  <- drain 중이라 새 연결도 받는다
sleep 3
curl -i http://127.0.0.1:8080/         # connection refused
```

실제 실행 결과:

```
  drain 중 /readyz = 503
  drain 중 /       = 200
  drain 후 /       = connection refused
in-flight: HTTP 200 (5.002847s)
  exit code: 0

2026/07/27 23:15:16 listening on http://127.0.0.1:8080 (drain=3s, timeout=10s)
2026/07/27 23:15:17 /slow started (d=5s)
2026/07/27 23:15:17 readiness turned off; press Ctrl+C again to exit immediately
2026/07/27 23:15:22 /slow: timer fired -> responding normally
2026/07/27 23:15:22 shutdown complete
```

`timer fired` 가 핵심이다. 종료가 진행되는 5초 내내 `r.Context()` 는 살아 있었다.
**`Shutdown` 은 in-flight 요청의 context 를 취소하지 않는다.**

### 2. 유예 초과 -> 강제 종료

```bash
/tmp/gsdemo -drain 0s -timeout 1s
curl -v "http://127.0.0.1:8080/slow?d=30s"   # 다른 터미널에서 Ctrl+C
```

```
in-flight: curl exit=52 HTTP 000 (1.495901s)     # 52 = Empty reply from server
  exit code: 1

2026/07/27 23:15:38 /slow: request canceled -> context canceled
2026/07/27 23:15:39 forced shutdown; some requests did not finish in time:
                    gracefulshutdown: shutdown timed out: context deadline exceeded
```

이번엔 `request canceled` 다. 유예가 끝나면서 hard stop 이 핸들러까지 전달됐고,
에러는 `ErrShutdownTimeout` 과 `context.DeadlineExceeded` 를 모두 감싸고 있다.
`/slow` 는 이 경로에서 응답 본문을 쓰지 않고 반환하므로 curl 은 `exit=52`(empty reply)
를 본다. 타임아웃을 초과하면 응답이 끊기는 것이 정상이다.

### 3. 두 번째 Ctrl+C 로 긴 drain 탈출

```bash
/tmp/gsdemo -drain 30s
# Ctrl+C  -> 30초 drain 시작
# Ctrl+C  -> 즉시 종료
```

```
2026/07/27 23:15:39 listening on http://127.0.0.1:8080 (drain=30s, timeout=10s)
2026/07/27 23:15:40 readiness turned off; press Ctrl+C again to exit immediately
  exit code: 130   # 128 + SIGINT(2). 기본 동작이 되살아났다는 증거다
  소요: 2초
```

## 함정 모음

### 1. `http.ErrServerClosed` 는 정상 종료다 - 구현됨

`srv.Serve` 는 종료할 때 이 에러를 돌려준다. 그대로 흘리면 성공한 배포가 매번 실패로 기록된다.

다만 **무조건** 삼키면 반대쪽으로 넘어진다. 종료 신호 없이 이 에러가 왔다면 이미 닫힌 `srv` 를
넘겨받았거나 누군가 밖에서 `srv.Shutdown`/`srv.Close` 를 부른 것이다. 그때 nil 을 돌려주면
서버가 아무것도 하지 않은 채 성공으로 보고된다. 그래서 `ctx` 가 취소된 뒤에 온 것만 정규화한다.

### 2. 취소된 context 로 `WithTimeout` 하지 마라 - 구현됨

가장 흔한 버그인데 코드는 멀쩡해 보인다.

```go
// 잘못된 코드. ctx 는 시그널로 이미 취소된 상태다.
shutdownCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
srv.Shutdown(shutdownCtx)   // 즉시 DeadlineExceeded 로 반환된다
```

`context.WithoutCancel(ctx)` 로 취소만 끊고 값은 유지한다.

### 3. `Shutdown` 은 무한히 기다린다 - 구현됨

godoc 그대로다: *"waiting **indefinitely** for connections to return to idle"*.
타임아웃을 걸고 초과 시 `Close()` 로 끊어야 `terminationGracePeriodSeconds` 안에 프로세스가 끝난다.

**그런데 타임아웃이 못 막는 구간이 하나 있다.** `Shutdown` 은 `listenerGroup.Wait()` 를 ctx 와
무관하게 기다린 **다음에야** ctx 를 감시하는 폴링 루프로 들어간다. 그래서 `ln.Close()` 가
`Accept` 를 깨우지 못하는 리스너를 넘기면 유예를 걸어 두어도 그 구간에서 멈춘다.
`srv.Close()` 도 같은 `Wait` 를 하고, 리스너는 `onceCloseListener` 라 두 번째 `Close()` 가
no-op 이므로 탈출구도 없다.

이 스니펫은 hard stop 만 `context.AfterFunc` 으로 떼어 내 마감 시각에 정확히 전달한다.
**얻는 것은 "핸들러가 제때 정리 기회를 받고 그 사실이 반환값에 정직하게 반영된다" 뿐이고,
반환 시각은 여전히 리스너 구현에 달려 있다.** 평범한 `net.TCPListener` 라면 문제되지 않는다.

### 4. `Shutdown` 은 `r.Context()` 를 취소하지 않는다 - 구현됨

이건 버그가 아니라 설계다. 그래야 in-flight 요청이 끝까지 완료된다.
다만 핸들러가 "지금 종료 중"임을 알 방법이 없어지므로, 이 스니펫은 `srv.BaseContext` 에
hard stop context 를 심어 **유예가 끝나는 순간에만** 모든 `r.Context()` 를 취소한다.
(hijack 된 커넥션은 예외다. 함정 7 참고.)

### 5. Kubernetes / 로드밸런서 디레지스터 레이스 - 구현됨

Pod 종료 시 "SIGTERM 전달"과 "Endpoints 에서 제거"는 **병렬로** 일어난다.
SIGTERM 을 받자마자 리스너를 닫으면 아직 나를 가리키는 LB 가 보낸 트래픽이 거부된다.

`DrainDelay` 로 그 시간을 벌고, `OnShutdownStart` 에서 readiness 를 503 으로 내린다.
`preStop` hook 으로도 같은 효과를 낼 수 있지만, 코드 안에 두면 어디에 배포하든 동작한다.
`terminationGracePeriodSeconds` 는 `DrainDelay + ShutdownTimeout` 보다 넉넉해야 한다.

훅은 `srv.Serve` 가 종료 신호보다 먼저 죽은 경로에서도 불린다. 그 경로에서 readiness 를
안 내리면 in-flight 를 비우는 내내 `/readyz` 가 200 을 반환하는데, readiness 를 별도
리스너나 사이드카로 서비스하는 구성에서는 LB 가 계속 트래픽을 보내 502 가 난다.

### 6. keep-alive idle 커넥션 - 설명만

직접 처리할 게 없다. `doKeepAlives()` 가 `!shuttingDown()` 을 보기 때문에
`Shutdown` 이 시작되면 응답에 `Connection: close` 가 자동으로 붙고 idle 커넥션은 바로 닫힌다.
`SetKeepAlivesEnabled(false)` 를 직접 부를 필요 없다.

클라이언트 쪽 레이스는 남는다. 서버가 idle 커넥션을 닫는 순간 클라이언트가 그 커넥션으로
요청을 보내면 EOF 를 받는다. `http.Transport` 는 idempotent 요청만 자동 재시도한다.

### 7. hijacked 커넥션 (WebSocket, SSE) - 설명만

godoc: *"Shutdown does not attempt to close nor wait for hijacked connections such as WebSockets."*
`Close()` 도 마찬가지다. 애플리케이션이 커넥션 레지스트리를 들고 종료를 브로드캐스트한 뒤
`sync.WaitGroup` 으로 기다려야 한다.

`RegisterOnShutdown` 이 그 알림 지점이지만 **완료를 기다려 주지 않는다**.
`Shutdown` 이 훅을 `go f()` 로 띄우고 바로 다음으로 넘어가기 때문이다.

**hijack 된 `r.Context()` 에 의존하면 안 된다.** 두 시점에 죽는다.

1. 핸들러가 `ServeHTTP` 를 반환하면 net/http 가 **그 즉시** 취소한다.
   `cancelCtx` 필드 주석이 *"when ServeHTTP exits"* 라고 적혀 있는 그대로다.
2. hijack 후에도 핸들러가 계속 돌고 있으면 `Serve` 가 반환할 때 hard stop 이 취소한다.
   `setState(StateHijacked)` 가 커넥션을 `activeConn` 에서 빼 버려 `Shutdown` 이 기다려 주지
   않으므로, 이 일은 **정상 graceful 종료에서도** 일어난다.

어느 쪽이든 커넥션 수명보다 먼저 죽는다. 종료 신호도 대기도 애플리케이션이 소유한 context 로 해야 한다.

### 8. 종료 순서: HTTP -> 백그라운드 워커 -> DB/큐 - 설명만

워커를 시그널 context 로 서버와 같이 죽이면 안 된다.
in-flight 핸들러가 그 워커에 의존하고 있으면 의존 대상이 먼저 죽는다.

```go
// 워커는 시그널 context 와 분리한다.
workerCtx, stopWorkers := context.WithCancel(context.Background())
var wg sync.WaitGroup
wg.Go(func() { runWorker(workerCtx) })

err := gracefulshutdown.Serve(ctx, srv, ln, cfg)  // HTTP 가 완전히 끝난 뒤에
stopWorkers()                                     // 워커를 멈춘다
wg.Wait()
```

hijack 된 커넥션의 종료 브로드캐스트도 여기, 즉 앱이 소유한 context 쪽에 둔다.
`Serve` 가 반환한 뒤에는 `r.Context()` 가 이미 죽어 있다 - 함정 7.

### 9. 두 번째 Ctrl+C 를 살리려면 stop() 을 불러라 - cmd 에서 구현

`signal.NotifyContext` 가 시그널을 가로채고 있으므로 두 번째 Ctrl+C 는 그냥은 무시된다.
godoc 은 `stop()` 이 *"like signal.Reset, **may** restore the default behavior"* 라고만 하는데,
**실제로 되살린다.** 등록된 채널이 하나도 남지 않으면 `signal.Stop` 이 `signal_disable` 을 부르고,
그러면 `sigsend` 가 false 를 반환하며, SIGINT/SIGTERM 은 런타임 sigtable 에서 `_SigKill` 이라
`dieFromSignal` 로 넘어간다. darwin 과 linux 모두 같다.

```go
ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

// 첫 시그널이 오는 즉시 핸들러를 해제한다. 두 번째부터는 기본 동작이 프로세스를 죽인다.
// 훅 안에서 하면 Serve 가 깨어날 때까지 늦어진다.
context.AfterFunc(ctx, stop)
```

그래도 godoc 의 "may" 는 진짜 hedge 다. 되살아나지 않는 경우가 셋 있다.

1. **다른 `signal.Notify` 가 같은 시그널에 남아 있으면** `stop()` 은 아무 일도 하지 않는다.
   `signal.Stop` 은 `handlers.ref[n]` 이 0 이 될 때만 `signal_disable` 을 부르기 때문이다.
   두 번째 시그널을 직접 받겠다고 `signal.Notify` 를 걸어 둔 채로 관측하면
   "`stop()` 은 소용없다" 는 잘못된 결론에 이르기 쉽다.
2. **SIGINT 을 `SIG_IGN` 으로 물려받은 프로세스**에서는 해제가 '무시'로 되돌린다.
   `sigInstallGoHandler` 가 SIGHUP/SIGINT 에 한해 상속된 `SIG_IGN` 을 존중하기 때문이다.
   비대화형 셸의 백그라운드 잡이 정확히 이 경우라 `prog & kill -INT` 로는 재현되지 않는다.
   `( trap - INT; exec prog ) &` 로 띄우거나 SIGTERM 을 쓰면 재현된다.
3. 시그널 도착과 `stop()` 실행 사이에는 goroutine 두 번의 창이 있다. 그 사이에 온 시그널은
   `NotifyContext` 내부 채널(버퍼 1)에만 들어가고, 첫 시그널을 받은 내부 goroutine 은 이미
   종료했으므로 아무도 읽지 않는다. `NotifyContext` 를 쓰는 대가다.

### 10. TLS

`tls.NewListener(ln, tlsConfig)` 로 감싼 리스너를 그대로 넘기면 된다.
HTTP/2 라면 `Shutdown` 이 GOAWAY 를 보내 클라이언트가 새 스트림을 열지 않게 한다.

## 테스트 전략

`go test -race -count=10` 을 통과하는 결정적 테스트 15개. `time.Sleep` 으로 "이쯤이면 됐겠지" 하는
곳이 한 군데도 없다. 종료 절차의 각 지점을 채널로 붙잡아 고정한다.

**리스너를 주입받는 설계가 테스트를 가능하게 한다.** 리스너를 래핑해 `Close()` 를 가로채면
"shutdown 이 리스너까지 닫았다"는 시점을 공개 API 만으로 정확히 알 수 있다.

```go
func (l *notifyListener) Close() error {
    err := l.Listener.Close()          // 실제 소켓을 먼저 닫고
    l.once.Do(func() { close(l.closed) }) // 그다음에 알린다
    return err
}
```

순서가 중요하다. 채널을 먼저 닫으면 소켓이 아직 살아 있는 동안 테스트가 dial 해서
"새 연결 거부" 검증이 간헐적으로 실패한다. `sync.Once` 도 필수다 -
`srv.Serve` 의 `defer` 와 테스트 cleanup 양쪽에서 `Close()` 가 불린다.
같은 래핑 기법으로 `Close()` 가 에러를 반환하는 리스너를 만들면 `ErrListenerClose` 경로도 고정된다.

**훅을 블록시켜 drain 구간을 고정한다.** `OnShutdownStart` 가 동기 호출이므로,
테스트가 훅 안에서 멈추면 "readiness 는 내려갔지만 리스너는 아직 열린" 상태를 확정적으로 붙잡을 수 있다.
반대로 훅 **안에서 동기로** `AbortDrain` 을 닫거나 `srv.Close()` 를 부르면 "drain 에 들어서는
순간 이미 조건이 성립한" 상태를 만들 수 있어 조기 기상 경로도 타이머 경합 없이 검증된다.

**타이머를 써도 flaky 하지 않게.** 유예 초과 테스트의 핸들러는 hard stop 전에는 절대 반환하지 않으므로
머신이 아무리 느려도 타임아웃은 항상 초과된다. `100ms` 는 테스트 소요 시간에만 영향을 주고
판정에는 영향을 주지 않는다. drain 시간 단언은 양방향 모두 안전하다 - 하한 단언(`50ms` 이상
기다렸는가)은 느린 머신이 elapsed 를 **키우기만** 하므로 거짓 실패 방향이 아니고, 조기 기상의
상한 단언(`10s` 미만에 끝났는가)은 정상 구현이 수 ms 에 끝나므로 여유가 압도적이다.
**이 상한 단언이 없으면 `AbortDrain` 을 아예 보지 않는 구현도 통과한다.** 그런 구현은 타이머
만료까지 기다렸다가 결국 `nil` 을 돌려주기 때문이다.

**핸들러가 관측한 값은 반드시 채널로 넘긴다.** 공유 변수에 쓰고 테스트에서 읽으면 `-race` 가 터진다.
소켓 I/O 는 race detector 가 인정하는 happens-before 간선이 아니기 때문이다.
채널은 **버퍼 1** 이어야 한다. 무버퍼면 테스트가 다른 채널을 먼저 기다릴 때 서로를 붙잡는다.

**핸들러 안의 `close()` 는 `sync.OnceFunc` 으로 감싼다.** 지금은 요청이 정확히 한 번뿐이라
그냥 `close()` 해도 통과하지만, `http.Transport` 의 재시도나 나중에 추가될 요청 하나로
"close of closed channel" 이 나면 해당 테스트 실패가 아니라 **테스트 바이너리 전체가 죽어**
원인 추적이 어려워진다.

**요청 goroutine 안에서 `t.Fatal` 을 부르면 안 된다.** `go vet` 의 `testinggoroutine` 이 막는다.
값만 채널로 넘기고 판정은 테스트 goroutine 에서 한다.

**테스트마다 새 `http.Transport` 를 쓴다.** idle 커넥션이 재사용되면
"새 연결이 거부되는가" 와 "drain 중에도 새 연결을 받는가" 두 검증이 모두 무의미해진다.

### `testing/synctest` 를 쓰지 않은 이유

Go 1.25+ 의 `testing/synctest` 는 가짜 시계로 타이머 테스트를 즉시 끝내 준다.
하지만 패키지 문서가 *"Avoid using the network. Use a fake network implementation as needed."*
라고 명시하고, 소켓 읽기 블로킹은 durably blocking 이 아니라고 못박는다.
실제 loopback 리스너를 bubble 안에서 쓰면 bubble 이 idle 에 도달하지 못해 가짜 시계가 전진하지 않는다.

이 스니펫은 실제 리스닝 주소가 있어야 성립하고, 위 설계는 이미 sleep 없이 결정적이라
synctest 로 얻을 이득이 벽시계 대기 150ms 제거뿐이다. 비용 대비 이득이 없다.

## 참고 자료

- [`net/http.Server.Shutdown`](https://pkg.go.dev/net/http#Server.Shutdown)
- [`context.WithoutCancel`](https://pkg.go.dev/context#WithoutCancel) / [`context.AfterFunc`](https://pkg.go.dev/context#AfterFunc)
- [`os/signal.NotifyContext`](https://pkg.go.dev/os/signal#NotifyContext) / [`os/signal.Stop`](https://pkg.go.dev/os/signal#Stop)
- [Kubernetes: Pod 종료](https://kubernetes.io/docs/concepts/workloads/pods/pod-lifecycle/#pod-termination)

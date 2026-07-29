# Kubernetes Leader Election (Lease)

Kubernetes **Lease 오브젝트**로 여러 replica 중 **하나만 리더**로 선출하는 코드 스니펫.

## 무엇을 해결하나

같은 앱을 replica 여러 개로 띄웠는데, 그중 "동시에 하나만" 돌아야 하는 작업이 있을 때가 있다.
크론 잡, 리컨실러 루프, 외부로 나가는 싱글톤 워커, 캐시 워머 같은 것들이다. replica 를 늘려
가용성은 확보하되, 그 특정 작업만큼은 한 인스턴스에서만 실행하고 싶은 상황이다.

Kubernetes 는 이를 위한 분산 락 프리미티브로 `coordination.k8s.io` 의 **Lease** 오브젝트를
제공한다. client-go 의 `tools/leaderelection` 이 그 위에서 임대 획득 - 갱신 - 상실의 생명주기를
대신 굴려주므로, 우리는 "리더가 됐을 때 / 잃었을 때" 콜백만 채우면 된다.

```
        replica 3 개가 하나의 Lease 를 두고 경쟁

   pod-a  ---\
   pod-b  ----+-->  Lease "snippetgo-leaderelection"  --> holderIdentity: pod-a
   pod-c  ---/          (coordination.k8s.io)

   pod-a: started leading  -> 싱글톤 작업 수행
   pod-b: new leader: pod-a -> 대기
   pod-c: new leader: pod-a -> 대기

   pod-a 가 죽으면 -> 임대 만료/반납 -> pod-b 또는 pod-c 가 이어받음(failover)
```

## 빠른 시작

kind + Docker 로 끝까지 한 번에 돌려본다(정리 -> 클러스터 구축 -> 이미지 빌드/적재 -> 배포):

```bash
cd k8s/leaderelection
make demo
make logs     # 한 Pod 만 "started leading" 을 남긴다
```

라이브러리로 쓸 때:

```go
import "github.com/daengdaengLee/snippetgo/k8s/leaderelection"

err := leaderelection.Run(ctx, client, leaderelection.Config{
    Namespace: "my-ns",
    LeaseName: "my-app",
    Identity:  os.Getenv("POD_NAME"), // 그룹 안에서 유일해야 한다
    OnStartedLeading: func(ctx context.Context) {
        // 리더 전용 작업. ctx 취소 시 반드시 멈춰야 한다.
    },
    OnStoppedLeading: func() { /* 리더 자원 정리 */ },
})
```

## API

```go
func Run(ctx context.Context, client kubernetes.Interface, cfg Config) error
func RunUntilCancelled(ctx context.Context, client kubernetes.Interface, cfg Config) error
```

`Run` 은 리더 선출 **한 세션**에 참여해, `ctx` 취소 **또는** 리더직 비자발적 상실(갱신 실패)
중 먼저 오는 시점에 반환한다. 정상 취소(SIGTERM 등)면 `nil`, 리더직을 잃으면 `ErrLostLease` 다.

`RunUntilCancelled` 는 `Run` 을 반복 호출해, 리더직을 잃어도 **같은 프로세스에서 재경쟁**하며
`ctx` 취소까지 블록한다. 리더직 상실을 프로세스 종료로 다루려면(재시작 위임) `Run` 을, 인프로세스
재선출을 원하면 `RunUntilCancelled` 를 쓴다 (함정 6 참고).

`Config` 필드:

| 필드 | 필수 | 기본값 | 설명 |
| --- | --- | --- | --- |
| `Namespace` | O | - | Lease 가 생성/조회되는 네임스페이스 |
| `LeaseName` | O | - | 경쟁 그룹이 공유하는 Lease 이름(모두 같은 값) |
| `Identity` | O | - | 이 인스턴스의 고유 식별자(보통 Pod 이름) |
| `LeaseDuration` | X | `15s` | 리더가 죽은 뒤 임대를 빼앗기까지의 상한(=failover 지연) |
| `RenewDeadline` | X | `10s` | 리더가 이 시간 안에 갱신 실패 시 스스로 리더직을 내려놓음 |
| `RetryPeriod` | X | `2s` | 획득/갱신 재시도 간격 |
| `OnStartedLeading` | X | nil | 리더가 됐을 때 호출. 넘어온 ctx 는 리더직 상실 시 취소됨 |
| `OnStoppedLeading` | X | nil | Run 종료 시 항상 호출(리더가 된 적 없어도). 정리는 멱등이어야 함 (함정 7) |
| `OnNewLeader` | X | nil | 관찰된 리더가 바뀔 때마다 호출(관측용) |

에러 계약:

| 반환 | 언제 |
| --- | --- |
| `nil` | `ctx` 가 정상 취소되어 종료 |
| `ErrLostLease` | (`Run` 한정) 리더였다가 갱신 실패로 리더직을 비자발적으로 상실 |
| `ErrInvalidConfig` | `Namespace`/`LeaseName`/`Identity` 중 하나라도 비었을 때 |
| 그 외 에러 | 타이밍 관계식 위반, 클라이언트 생성 실패 등 |

## 동작 방식

리더 선출의 핵심은 세 타이밍 값의 관계다. `RenewDeadline < LeaseDuration` 이어야 하고,
`RenewDeadline` 은 단순히 `RetryPeriod` 보다 큰 게 아니라 **`RetryPeriod * JitterFactor(1.2)`
보다** 커야 한다. 이 관계식은 client-go `NewLeaderElector` 가 검사하고, 어기면 `Run` 이 그
에러를 그대로 반환한다(함정 2 참고).

| 값 | 의미 | 크면 | 작으면 |
| --- | --- | --- | --- |
| `LeaseDuration` | 임대 유효 기간. 리더가 갱신을 멈춘 뒤 이 시간이 지나야 남이 가져감 | failover 느림, API 부하 적음 | failover 빠름, 오탐 위험 |
| `RenewDeadline` | 현재 리더가 이 안에 갱신 못 하면 자진 하야 | 리더 유지 관대 | 네트워크 지연에 민감 |
| `RetryPeriod` | 획득/갱신 시도 간격 | API 부하 적음 | 반응 빠름, 부하 큼 |

`Run` 은 종료 시(`ctx` 취소) 리더였다면 **`ReleaseOnCancel` 로 Lease 를 즉시 반납**한다.
그래서 정상 종료(파드 rolling update, SIGTERM) 에서는 다음 리더가 `LeaseDuration` 만료를
기다리지 않고 바로 이어받는다. 반대로 리더 Pod 가 갑자기 죽으면(kill -9, 노드 장애) 반납이
없으므로 남은 인스턴스는 `LeaseDuration` 이 지나서야 임대를 가져간다.

## 직접 확인해보기

| 명령 | 하는 일 |
| --- | --- |
| `make demo` | 기존 클러스터 정리 -> kind 생성 -> 이미지 빌드/적재 -> 배포 (`exit` 모드) |
| `make demo-rejoin` | 위와 같되 rejoin overlay 로 배포 |
| `make logs` | 모든 Pod 로그 실시간(리더 관찰) |
| `make status` | Lease 1 개와 Pod 3 개 상태 |
| `make leader` | 현재 리더(`Lease.holderIdentity`) 출력 |
| `make kill-leader` | 현재 리더 Pod 삭제 -> failover 유도 |
| `make clean` | 클러스터/바이너리 정리 |

리더직 상실 처리 방식(`LE_MODE`)은 kustomize 로 고른다. base(`manifests/base`)는 `exit`,
`manifests/overlays/rejoin` overlay 가 `rejoin` 으로 덮어쓴다. `make deploy` 는
`kubectl apply -k $(KUSTOMIZE_DIR)`(기본 `manifests/base`) 로 배포하고, `make demo-rejoin` 은
overlay 를 가리킨다.

배포 후 Lease 를 보면 한 Pod 가 홀더로 잡혀 있다:

```
$ make status
NAME                                                  HOLDER                        AGE
lease.coordination.k8s.io/snippetgo-leaderelection    leaderelection-6c9d4f8b7-abcde 12s

NAME                                  READY   STATUS    RESTARTS   AGE
pod/leaderelection-6c9d4f8b7-abcde    1/1     Running   0          12s
pod/leaderelection-6c9d4f8b7-fghij    1/1     Running   0          12s
pod/leaderelection-6c9d4f8b7-klmno    1/1     Running   0          12s
```

로그는 리더 하나만 작업을 돌리고 나머지는 대기한다:

```
$ make logs
[leaderelection-6c9d4f8b7-abcde] started leading - 여기서 싱글톤 작업을 돌린다
[leaderelection-6c9d4f8b7-abcde] 리더 작업 수행 중...
[leaderelection-6c9d4f8b7-fghij] new leader: leaderelection-6c9d4f8b7-abcde (나는 대기)
[leaderelection-6c9d4f8b7-klmno] new leader: leaderelection-6c9d4f8b7-abcde (나는 대기)
```

failover 관찰 - 리더를 죽이면 수 초 안에 다른 Pod 가 이어받는다:

```
$ make kill-leader
pod "leaderelection-6c9d4f8b7-abcde" deleted
# make logs 에서:
[leaderelection-6c9d4f8b7-fghij] started leading - 여기서 싱글톤 작업을 돌린다
```

`ReleaseOnCancel` 덕에 정상 종료(`delete pod` 는 SIGTERM 을 보냄) 라 Lease 가 즉시 반납되어
`LeaseDuration` 을 기다리지 않고 바로 넘어간다. 이 `kill-leader` failover 는 SIGTERM -> ctx 취소
-> 깨끗한 종료 경로라 **exit/rejoin 두 모드에서 동일**하게 동작한다. 두 모드의 차이는 *비자발적*
리더직 상실(리더가 API 서버와 단절돼 갱신에 실패하는 경우) 에서만 드러나는데, 이건 API 파티션을
인위로 만들어야 해서 이 데모 스크립트로는 재현하지 않는다(함정 6 에서 설명).

## 매니페스트 / RBAC

`manifests/` 는 표준 base/overlays 구조다. `manifests/base/` 가 base(namespace/rbac/deployment
+ kustomization), `manifests/overlays/rejoin/` 이 overlay 다.

- `base/namespace.yaml` - 전용 네임스페이스 `snippetgo-leaderelection`.
- `base/rbac.yaml` - ServiceAccount + Role + RoleBinding. Role 은 **`coordination.k8s.io` 의
  `leases` 에 대해 `get,create,update` 만** 준다. `resourcelock.LeaseLock` 은 단일 Lease 를
  get(확인)/create(최초)/update(갱신,반납)할 뿐 list/watch/patch/delete 를 쓰지 않는다.
  `EventRecorder` 도 안 써서 `events` 권한 역시 필요 없다.
- `base/deployment.yaml` - `replicas: 3`. Downward API 로 `POD_NAME`/`POD_NAMESPACE` 를 주입해
  identity 로 쓴다. 이미지는 `imagePullPolicy: IfNotPresent`(kind 로 노드에 직접 적재),
  distroless nonroot 와 맞춘 `securityContext`(비루트, 읽기 전용 루트 FS).
- `overlays/rejoin/` - `LE_MODE` 만 `rejoin` 으로 바꾸는 overlay(base 를 참조).
  `kubectl apply -k manifests/base`(exit) / `kubectl apply -k manifests/overlays/rejoin`(rejoin).

이미지는 로컬에서 `go build` 한 정적 바이너리를 `Dockerfile` 이 복사만 한다(빌드는 Makefile,
런타임은 `gcr.io/distroless/static:nonroot`).

## 함정 모음

### 1. 리더 선출은 상호 배제(fencing) 가 아니다 - 설명만

시계 오차나 네트워크 지연으로 옛 리더가 자기 상실을 늦게 인지하면 **아주 짧은 순간 리더가
둘일 수 있다**. 진짜로 "절대 동시에 둘이면 안 되는" 자원은 리더 신분과 별개로 fencing 을 걸어야
한다(예: 쓰기 시 리소스 버전/펜싱 토큰 검증). 리더 선출은 "대부분의 시간에 하나" 를 보장할 뿐이다.

### 2. 타이밍 검증은 client-go 가 최종 권위 - 위임

타이밍 관계식(`LeaseDuration > RenewDeadline`, `RenewDeadline > RetryPeriod * JitterFactor(1.2)`)은
client-go `NewLeaderElector` 가 강제한다. 예를 들어 `RetryPeriod=9s, RenewDeadline=10s` 는 단순
대소로는 통과하지만 `1.2*9=10.8s >= 10s` 라 거부된다. 이 래퍼의 `validate` 는 기본값으로 못
채우는 필수 필드(Namespace/LeaseName/Identity)만 확인하고, 타이밍은 중복 검사하지 않는다 -
어긋나면 `NewLeaderElector` 의 에러가 `Run` 을 통해 그대로 나온다(단일 권위, 상수 변경에도 자동 정합).

### 3. `ReleaseOnCancel` 로 빠른 failover - 구현됨

`Run` 은 `ReleaseOnCancel: true` 라, `ctx` 취소 시 리더면 Lease 를 즉시 반납한다. 이게 없으면
정상 종료에서도 다음 리더가 `LeaseDuration` 만큼 기다린다. 단, 프로세스가 갑자기 죽으면(SIGKILL)
반납할 새가 없어 만료를 기다리는 건 어쩔 수 없다.

### 4. identity 는 그룹 안에서 유일해야 한다 - cmd 에서 구현

두 인스턴스가 같은 identity 를 쓰면 서로를 자신으로 오인해 임대가 엉킨다. 파드 안에서는 Downward
API 의 `metadata.name`(Pod 이름) 을 `POD_NAME` 으로 주입해 쓴다. `cmd/main.go` 는 `POD_NAME`
이 없으면 호스트네임으로 대체한다.

### 5. `OnStartedLeading` 의 ctx 를 존중하라 - cmd 에서 구현

리더 전용 작업은 콜백으로 넘어온 `ctx` 가 취소되면(리더직 상실/종료) **반드시 멈춰야 한다**.
멈추지 않으면 리더가 바뀐 뒤에도 옛 리더가 작업을 계속해 함정 1 을 악화시킨다. `cmd/main.go` 의
`runLeaderWork` 는 `ticker` 루프에서 `ctx.Done()` 을 항상 확인한다. 다만 client-go 가
`OnStartedLeading` 을 detached 고루틴으로 띄우므로, rejoin 모드에서 같은 Pod 가 상실 직후
재획득하면 이전 작업 고루틴이 완전히 끝나기 전 새 고루틴과 아주 짧게 겹칠 수 있다. 이 겹침은
코드로 완전히 없앨 수 없으니, 실전 싱글톤 작업이라면 멱등성/펜싱으로 중복 수행을 흡수해야 한다.

### 6. 리더직 상실 처리: exit vs rejoin - 구현됨

client-go `LeaderElector.Run(ctx)` 은 "ctx 취소 **또는** 리더직 상실" 중 먼저 오는 시점에
반환한다. 리더가 갱신에 실패해 임대를 놓치면 ctx 가 살아 있어도 반환한다. 이 스니펫은 두 처리
방식을 모두 제공한다.

- **exit** (`Run`, 기본): 상실 시 `ErrLostLease` 반환 -> `cmd` 가 프로세스를 비정상 종료 ->
  Kubernetes 가 Pod 를 재시작해 **깨끗한 상태로** 다시 경쟁에 합류. kube-controller-manager,
  controller-runtime 이 쓰는 표준 방식. 리더가 in-memory 상태를 쌓아둔다면 이쪽이 안전하다.
  단, 갱신 실패가 짧은 간격으로 반복되면 kubelet 의 CrashLoopBackOff 지수 백오프(최대 약 5분)에
  걸려 그 Pod 의 재합류가 지연될 수 있다.
- **rejoin** (`RunUntilCancelled`): 상실해도 **같은 프로세스가 후보로 재참여**한다. 재시작
  비용이 없지만, 이전 리더의 in-memory 상태가 남은 채 재경쟁하므로 함정 5(ctx 존중) 를 특히
  철저히 지켜 리더 전환 시 상태를 확실히 리셋해야 한다.

`-mode`(또는 `LE_MODE`) 로 고르고, `make demo` / `make demo-rejoin` 으로 각각 배포해 볼 수 있다.

### 7. OnStoppedLeading 은 리더가 아니어도 호출된다 - 설명만

client-go 콜백 문서가 명시한다: "OnStoppedLeading ... is always called when the LeaderElector
exits, even if it did not start leading. Users should not assume that OnStoppedLeading is only
called after OnStartedLeading." 즉 리더가 된 적 없는 팔로워가 종료해도 이 콜백이 불린다
(`Run` 안 defer). 따라서 **OnStoppedLeading 의 정리 로직은 멱등이어야 하고, OnStartedLeading 이
먼저 실행됐다고 가정하면 안 된다**(예: OnStartedLeading 에서 연 핸들을 닫는다면 nil 여부를 먼저
확인). 이 순서 보장은 래퍼로 흉내낼 수 없다 - OnStartedLeading/OnNewLeader 는 client-go 가 별도
goroutine 으로 띄우고 OnStoppedLeading 만 메인 goroutine defer 라, "리더였는지" 를 race 없이
알려주는 동기 신호가 없기 때문이다. 그래서 이 스니펫은 client-go 계약을 그대로 노출한다.

## 참고 자료

- [client-go leaderelection 패키지](https://pkg.go.dev/k8s.io/client-go/tools/leaderelection)
- [Lease API (coordination.k8s.io)](https://kubernetes.io/docs/concepts/architecture/leases/)
- [Simple leader election with Kubernetes](https://kubernetes.io/blog/2016/01/simple-leader-election-with-kubernetes/)
- [Downward API 로 Pod 정보 노출](https://kubernetes.io/docs/tasks/inject-data-application/environment-variable-expose-pod-information/)

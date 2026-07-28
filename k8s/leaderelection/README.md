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
```

`ctx` 가 취소될 때까지 블록한다. 정상 취소(SIGTERM 등)면 `nil` 을 반환한다.

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
| `OnStoppedLeading` | X | nil | 리더직을 잃었을 때 호출 |
| `OnNewLeader` | X | nil | 관찰된 리더가 바뀔 때마다 호출(관측용) |

에러 계약:

| 반환 | 언제 |
| --- | --- |
| `nil` | `ctx` 가 정상 취소되어 종료 |
| `ErrInvalidConfig` | `Namespace`/`LeaseName`/`Identity` 중 하나라도 비었을 때 |
| 그 외 에러 | 타이밍 관계식 위반, 클라이언트 생성 실패 등 |

## 동작 방식

리더 선출의 핵심은 세 타이밍 값의 관계다. 항상
`RetryPeriod < RenewDeadline < LeaseDuration` 이어야 하며, 어기면 `Run` 이 에러를 낸다.

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
| `make demo` | 기존 클러스터 정리 -> kind 생성 -> 이미지 빌드/적재 -> 배포 |
| `make logs` | 모든 Pod 로그 실시간(리더 관찰) |
| `make status` | Lease 1 개와 Pod 3 개 상태 |
| `make leader` | 현재 리더(`Lease.holderIdentity`) 출력 |
| `make kill-leader` | 현재 리더 Pod 삭제 -> failover 유도 |
| `make clean` | 클러스터/바이너리 정리 |

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
`LeaseDuration` 을 기다리지 않고 바로 넘어간다.

## 매니페스트 / RBAC

`manifests/` 는 세 조각이다.

- `namespace.yaml` - 전용 네임스페이스 `snippetgo-leaderelection`.
- `rbac.yaml` - ServiceAccount + Role + RoleBinding. Role 은 **`coordination.k8s.io` 의
  `leases` 에 대한 권한만** 준다(`get,list,watch,create,update,patch,delete`). 리더 선출은
  Lease 를 만들고 자기 것으로 갱신하며 남의 상태를 볼 뿐이라, 그 외 권한은 필요 없다.
  이 스니펫은 `EventRecorder` 를 쓰지 않으므로 `events` 권한도 필요 없다.
- `deployment.yaml` - `replicas: 3`. Downward API 로 `POD_NAME`/`POD_NAMESPACE` 를 주입해
  identity 로 쓴다. 이미지는 `imagePullPolicy: IfNotPresent`(kind 로 노드에 직접 적재),
  distroless nonroot 와 맞춘 `securityContext`(비루트, 읽기 전용 루트 FS).

이미지는 로컬에서 `go build` 한 정적 바이너리를 `Dockerfile` 이 복사만 한다(빌드는 Makefile,
런타임은 `gcr.io/distroless/static:nonroot`).

## 함정 모음

### 1. 리더 선출은 상호 배제(fencing) 가 아니다 - 설명만

시계 오차나 네트워크 지연으로 옛 리더가 자기 상실을 늦게 인지하면 **아주 짧은 순간 리더가
둘일 수 있다**. 진짜로 "절대 동시에 둘이면 안 되는" 자원은 리더 신분과 별개로 fencing 을 걸어야
한다(예: 쓰기 시 리소스 버전/펜싱 토큰 검증). 리더 선출은 "대부분의 시간에 하나" 를 보장할 뿐이다.

### 2. `RetryPeriod < RenewDeadline < LeaseDuration` - 구현됨

이 관계식이 깨지면 `Run` 이 즉시 에러를 낸다(`validate`). client-go 도 내부에서 같은 조건을
강제하므로, 미리 걸러 명확한 메시지를 준다.

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
`runLeaderWork` 는 `ticker` 루프에서 `ctx.Done()` 을 항상 확인한다.

## 참고 자료

- [client-go leaderelection 패키지](https://pkg.go.dev/k8s.io/client-go/tools/leaderelection)
- [Lease API (coordination.k8s.io)](https://kubernetes.io/docs/concepts/architecture/leases/)
- [Simple leader election with Kubernetes](https://kubernetes.io/blog/2016/01/simple-leader-election-with-kubernetes/)
- [Downward API 로 Pod 정보 노출](https://kubernetes.io/docs/tasks/inject-data-application/environment-variable-expose-pod-information/)

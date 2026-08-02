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

## 사전 준비

`make demo` 는 Docker, kubectl, kind 를 쓴다. 라이브러리로만 쓸 거면 필요 없다.

```bash
# kind - Go 로 설치(버전을 고정할 수 있어 편하다)
go install sigs.k8s.io/kind@v0.32.0
export PATH=$PATH:$(go env GOPATH)/bin   # 설치 위치를 PATH 에 넣는다

# kind - 릴리스 바이너리로 설치할 때(위 대신)
# curl -Lo ./kind https://kind.sigs.k8s.io/dl/v0.32.0/kind-linux-amd64
# chmod +x ./kind && sudo mv ./kind /usr/local/bin/kind

# kubectl - 없다면
# curl -LO "https://dl.k8s.io/release/$(curl -sL https://dl.k8s.io/release/stable.txt)/bin/linux/amd64/kubectl"
# chmod +x kubectl && sudo mv kubectl /usr/local/bin/kubectl

# 확인
kind version
kubectl version --client
docker info          # 데몬이 떠 있어야 한다
```

`make demo` 는 kind 노드 이미지(`kindest/node`) 를 Docker Hub 에서 받는다. 사내 프록시나
격리된 네트워크라면 `docker.io` 와 그 blob CDN 접근이 열려 있어야 한다. macOS/Windows 는
Docker Desktop 이 떠 있으면 되고, 리눅스는 현재 사용자가 docker 그룹에 있어야 한다.

## 빠른 시작

kind + Docker 로 끝까지 한 번에 돌려본다(정리(클러스터/로컬 이미지) -> 클러스터 구축 ->
이미지 빌드/적재 -> 배포):

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
| `WaitForLeaderWork` | X | `false` | true 면 (리더였다면) OnStartedLeading 종료까지 기다린 뒤 반환 - 같은 프로세스 안 겹침만 (함정 5) |
| `OnStartedLeading` | X | nil | 리더가 됐을 때 호출. 넘어온 ctx 는 리더직 상실 시 취소됨 |
| `OnStoppedLeading` | X | nil | 선출 루프 진입 후 종료 시 항상 호출(리더가 된 적 없어도). 설정 오류로 조기 반환하면 미호출. 정리는 멱등이어야 함 (함정 7) |
| `OnNewLeader` | X | nil | 관찰된 리더가 바뀔 때마다 호출(관측용) |

에러 계약:

| 반환 | 언제 |
| --- | --- |
| `nil` | `ctx` 가 정상 취소(`context.Canceled`)되어 종료 |
| `ErrLostLease` | (`Run` 한정) 리더였다가 갱신 실패로 리더직을 비자발적으로 상실 |
| `ErrInvalidConfig` | `Namespace`/`LeaseName`/`Identity` 중 하나라도 비었을 때 |
| `ctx.Err()` | `ctx` 가 취소가 아닌 사유로 끝났을 때(데드라인 만료 등). 정상 종료(`nil`)와 뭉뚱그리지 않는다 |
| 그 외 에러 | 타이밍 관계식 위반 - client-go `NewLeaderElector` 의 에러를 그대로 전달한다 (함정 2) |

위 표에 없는 실패는 **에러로 반환되지 않는다.** RBAC `Forbidden`(예: Role 의 `resourceNames` 와
`LEASE_NAME` 불일치)이나 잘못된 Lease 이름처럼 획득 자체가 계속 거부되는 경우, client-go 의
`acquire` 는 실패해도 반환하지 않고 `RetryPeriod` 간격으로 무한 재시도한다. 그래서 `Run` 은
반환하지 않은 채 **블록한 상태로 남고**, 증상은 로그(`Error retrieving resource lock`)로만
드러난다. "리더 선출이 조용히 아무 일도 안 한다" 면 반환값이 아니라 Pod 로그를 봐야 한다.

반환값은 client-go 선출 루프가 반환한 시점에 확정된다. 그래서 `WaitForLeaderWork` 대기가
길어지는 동안 `ctx` 가 취소돼도 비자발적 상실은 `nil` 이 아니라 `ErrLostLease` 로 그대로 나온다.

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

정상 종료(파드 rolling update, SIGTERM) 에서는 리더가 Lease 를 반납하므로 다음 리더가
`LeaseDuration` 만료를 기다리지 않고 바로 이어받는다. 반대로 갑자기 죽으면(kill -9, 노드 장애)
반납이 없어 `LeaseDuration` 이 지나서야 넘어간다. 반납이 도는 조건은 함정 3 에서 다룬다.

## 직접 확인해보기

| 명령 | 하는 일 |
| --- | --- |
| `make demo` | 기존 클러스터/로컬 이미지 정리 -> kind 생성 -> 이미지 빌드/적재 -> 배포 (`exit` 모드) |
| `make demo-rejoin` | 위와 같되 rejoin overlay 로 배포 |
| `make deploy` | 빌드 -> 적재 -> apply -> Pod 재시작 -> 롤아웃 대기 (kind 클러스터 필수) |
| `make deploy-rejoin` | 클러스터 재생성 없이 rejoin overlay 재배포(빌드/적재/Pod 재시작 포함) |
| `make logs` | 모든 Pod 로그 실시간(리더 관찰) |
| `make status` | Lease 1 개와 Pod 3 개 상태 |
| `make leader` | 현재 리더(`Lease.holderIdentity`) 출력 |
| `make kill-leader` | 현재 리더 Pod 삭제 -> failover 유도 |
| `make break-renew` | Role 에서 `update` 회수 -> 비자발적 리더직 상실 유도 (함정 6) |
| `make fix-renew` | `rbac.yaml` 재적용으로 `update` 권한 복구 |
| `make clean` | 클러스터/로컬 이미지/바이너리 정리 |

모든 `kubectl` 호출은 `--context kind-snippetgo-le` 로 고정돼 있다. 이 Makefile 은 kind 전용이라
current-context 를 따라갈 이유가 없고, 따라가면 컨텍스트를 바꿔 둔 채 부른 `break-renew` 가
엉뚱한 클러스터의 RBAC 를 훼손하고 `kill-leader` 가 남의 Pod 를 지운다.

리더직 상실 처리 방식(`LE_MODE`)은 kustomize 로 고른다. base(`manifests/base`)는 `exit`,
`manifests/overlays/rejoin` overlay 가 `rejoin` 으로 덮어쓴다. `make deploy` 는 빌드 -> kind
노드 적재 -> `kubectl apply -k $(KUSTOMIZE_DIR)`(기본 `manifests/base`) -> Pod 재시작 -> 롤아웃
대기 순으로 돈다. 클러스터가 없으면 `make demo` 부터 시작한다.

`imagePullPolicy` 가 `Never` 라 노드 적재가 이미지를 넣는 유일한 경로다. 그래서 `deploy` 가
`load` 를 전제조건으로 갖는다. 또 태그가 `:dev` 로 고정이라 `apply` 만으로는 새 이미지가
반영되지 않으므로(spec 변화가 없어 롤아웃이 안 난다) `deploy` 가 `rollout restart` 까지 한다 -
그래서 `make deploy` 는 매번 Pod 를 새로 띄운다.

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
리더직 상실(리더가 갱신에 실패하는 경우) 에서만 드러난다.

비자발적 상실은 API 파티션을 만들지 않고도 재현할 수 있다. renew 는 Lease `update` 이므로
**Role 에서 `update` verb 만 회수하면 ctx 는 살아 있는 채 갱신만 실패**한다:

```bash
make break-renew    # Role rules[0] 의 verbs 를 [get] 으로 줄인다(update 회수)
# exit 모드:   리더 Pod 가 RESTARTS 1 (STATUS 는 Running 유지)
# rejoin 모드: RESTARTS 0, 같은 프로세스가 재획득을 계속 시도
make fix-renew      # 복구
```

`break-renew` 는 Role 을 훼손된 상태로 남긴다. 복구하지 않으면 그 클러스터에서는 **어떤 Pod 도
리더가 될 수 없다.** 복구는 `make fix-renew` 다 - `rbac.yaml` 을 그대로 재적용하므로 verb 목록이
매니페스트와 어긋날 일이 없고, `apply -k` 와 달리 Deployment 를 건드리지 않아 `LE_MODE`
(exit/rejoin) 도 그대로 유지된다.

주의: 훼손 중에는 반납(`release`)도 거부되므로 `make leader` 가 옛 홀더 이름을 그대로 보여준다.
정상처럼 보이니 `RESTARTS` 와 로그로 판단해야 한다. 자세한 차이는 함정 6 에서 설명한다.

## 매니페스트 / RBAC

`manifests/` 는 표준 base/overlays 구조다. `manifests/base/` 가 base(namespace/rbac/deployment
+ kustomization), `manifests/overlays/rejoin/` 이 overlay 다.

- `base/namespace.yaml` - 전용 네임스페이스 `snippetgo-leaderelection`.
- `base/rbac.yaml` - ServiceAccount + Role + RoleBinding. Role 은 **`coordination.k8s.io` 의
  `leases` 에 대해 `get,create,update` 만** 준다. `resourcelock.LeaseLock` 은 단일 Lease 를
  get(확인)/create(최초)/update(갱신,반납)할 뿐 list/watch/patch/delete 를 쓰지 않는다.
  `EventRecorder` 도 안 써서 `events` 권한 역시 필요 없다.
  최소 권한은 verb 와 resource **두 축**이라, `get,update` 는 `resourceNames` 로 Lease 이름
  하나까지 좁혔다. 규칙이 둘로 나뉜 이유는 **`create` 를 `resourceNames` 로 좁힐 수 없기**
  때문이다 - 생성 시점엔 그 이름의 오브젝트가 아직 없어서 매칭할 대상이 없다. 대신
  `resourceNames` 는 `deployment.yaml` 의 `LEASE_NAME` 과 **반드시 같아야 한다**. 어긋나면
  Lease get/update 가 Forbidden 이라 아무도 리더가 되지 못하니 양쪽에 주석을 달아 뒀다.
- `base/deployment.yaml` - `replicas: 3`. Downward API 로 `POD_NAME`/`POD_NAMESPACE` 를 주입해
  identity 로 쓴다. 이미지는 **`imagePullPolicy: Never`** - `make load` 로 kind 노드에 심은
  이미지만 쓰고 레지스트리는 아예 조회하지 않는다. 이미지가 없으면 엉뚱한 걸 당겨오는 대신
  `ErrImageNeverPull` 로 멈춘다. distroless nonroot 와 맞춘 `securityContext`(비루트, 읽기 전용 루트 FS).
- `overlays/rejoin/` - `LE_MODE` 만 `rejoin` 으로 바꾸는 overlay(base 를 참조).
  배포는 `make deploy`(exit) / `make deploy-rejoin`(rejoin) 을 쓴다. `Never` 때문에 맨
  `kubectl apply -k` 만으로는 뜨지 않으므로, 직접 apply 할 거면 `make load` 를 먼저 돌려야 한다.

이미지는 로컬에서 `go build` 한 정적 바이너리를 `Dockerfile` 이 복사만 한다(빌드는 Makefile,
런타임은 `gcr.io/distroless/static:nonroot`).

## 함정 모음

아래 서술 중 client-go 내부 동작에 기대는 부분(함정 3/5/7)은 **client-go v0.36.3** 소스로 확인한
것이다. 업그레이드할 때는 그 세 항목을 다시 확인할 것.

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

### 3. `ReleaseOnCancel` 로 빠른 failover (취소 전용은 아니다) - 구현됨

`Run` 은 `ReleaseOnCancel: true` 라, `ctx` 취소 시 리더면 Lease 를 즉시 반납한다. 이게 없으면
정상 종료에서도 다음 리더가 `LeaseDuration` 만큼 기다린다.

이름이 "OnCancel" 이지만 **취소 전용이 아니다.** client-go 는 renew 루프를 빠져나온 뒤 종료
사유를 구분하지 않고 반납을 시도하므로, 갱신 실패로 임대를 비자발적으로 놓친 경우에도 반납이
돈다. 그래서 반납은 best effort 로 봐야 한다 - 프로세스가 갑자기 죽으면(SIGKILL) 반납할 새가
없어 만료를 기다려야 하고, 반대로 일시적 API 장애가 회복된 직후라면 이미 남이 가져간 Lease 의
홀더를 자기 판단으로 비워 버릴 수도 있다. 훼손 중에는 반납 자체가 거부된다("직접 확인해보기"
의 `break-renew` 주의 참고).

### 4. identity 는 그룹 안에서 유일해야 한다 - cmd 에서 구현

두 인스턴스가 같은 identity 를 쓰면 서로를 자신으로 오인해 임대가 엉킨다. 파드 안에서는 Downward
API 의 `metadata.name`(Pod 이름) 을 `POD_NAME` 으로 주입해 쓴다. `cmd/main.go` 는 `POD_NAME`
이 없으면 호스트네임으로 대체한다.

### 5. `OnStartedLeading` 의 ctx 를 존중하라 - cmd 에서 구현 + 옵션 제공

리더 전용 작업은 콜백으로 넘어온 `ctx` 가 취소되면(리더직 상실/종료) **반드시 멈춰야 한다**.
멈추지 않으면 리더가 바뀐 뒤에도 옛 리더가 작업을 계속해 함정 1 을 악화시킨다. `cmd/main.go` 의
`runLeaderWork` 는 `ticker` 루프에서 `ctx.Done()` 을 항상 확인한다.

그런데 ctx 를 존중해도 겹침이 남는다. client-go 는 `OnStartedLeading` 을 detached 고루틴으로
띄우고 그 종료를 기다리지 않은 채 `Run` 이 반환하므로, rejoin 모드에서 같은 Pod 가 상실 직후
재획득하면 이전 작업 고루틴이 정리를 끝내기 전에 새 고루틴이 시작될 수 있다.

`Config.WaitForLeaderWork` 를 켜면 `Run` 은 (리더 작업이 시작됐었다면) 그 종료까지 기다린 뒤
반환한다. 켜기 전에 범위를 정확히 알아야 한다:

- **막아 준다**: 같은 프로세스에서 `RunUntilCancelled` 이 곧바로 시작하는 다음
  `OnStartedLeading`. 이게 전부다.
- **막아 주지 않는다**: Lease 반납(함정 3) 과 `OnStoppedLeading`(함정 7) 은 client-go
  `LeaderElector.Run` 안에서 처리돼 이 대기보다 **먼저** 끝난다. 그래서 다른 Pod 는 이전 리더
  작업이 정리를 끝내기 전에 리더가 될 수 있고, `OnStoppedLeading` 도 그 고루틴과 겹칠 수 있다.
- **대가**: 콜백이 ctx 취소를 존중하지 않으면 `Run` 이 무한히 블록한다(그래서 기본값 `false`).
- **건너뛰는 창**: 상위 ctx 가 이미 취소된 경우 하나뿐이다. client-go 의 acquire 는 상위 ctx 가
  죽어야만 실패하므로, `Run` 이 돌아온 시점에 ctx 가 살아 있다면 리더 작업 goroutine 은 반드시
  떠 있고 대기도 반드시 걸린다. 즉 이 옵션이 필요한 **상실 -> 재경쟁 경로에서는 보장**이고,
  건너뛰는 쪽은 어차피 다음 판이 없어 겹칠 것도 없는 경우다.
- **반환값은 흔들리지 않는다**: 종료 사유는 client-go 선출 루프가 반환한 시점에 확정하고,
  대기는 그다음이다. 그래서 기다리는 동안 상위 ctx 가 취소돼도 비자발적 상실은 정상 종료
  (`nil`) 로 둔갑하지 않고 `ErrLostLease` 로 남는다.

`cmd` 데모는 rejoin 모드에서만 켠다(`WaitForLeaderWork: *mode == "rejoin"`). `runLeaderWork` 가
ctx 취소를 확실히 따라 블록 위험이 없고, exit 은 상실 시 프로세스가 죽어 기다릴 이유가 없다.

어느 쪽을 고르든, 실전 싱글톤 작업이라면 멱등성/펜싱으로 중복 수행을 흡수해야 한다
(함정 1 과 같은 결론이다). 이 옵션은 겹침 창을 줄여줄 뿐 상호 배제를 보장하지 않는다.

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
  철저히 지켜 리더 전환 시 상태를 확실히 리셋해야 한다. 데모의 rejoin 모드가
  `WaitForLeaderWork` 를 켜고 도는 이유도 여기에 있다(그 옵션의 범위는 함정 5 참고).

`-mode`(또는 `LE_MODE`) 로 고르고, `make demo` / `make demo-rejoin` 으로 각각 배포해 볼 수 있다.
차이는 `make break-renew` 로 직접 볼 수 있다("직접 확인해보기" 참고).

| | exit | rejoin |
| --- | --- | --- |
| `RESTARTS` | 1 (exit code 1) | 0 |
| 프로세스 | 죽고 kubelet 이 재시작 | 그대로 살아 재획득 시도 |
| 로그 | `리더직 상실 - 재시작에 위임한다` | `OnStoppedLeading` 직후 `Attempting to acquire leader lease` |

두 가지를 덧붙인다.

- `break-renew` 실험에서 exit 모드 Pod 는 **재시작 1회로 끝나고 `Running` 에 머문다.** 위
  CrashLoopBackOff 서술의 전제는 "갱신 실패가 짧은 간격으로 **반복**되면" 인데, 이 실험은
  `update` 를 아예 막아 재시작된 Pod 가 리더가 되지도 못하게 하므로 두 번째 크래시가 없다
  (`acquire` 루프는 실패해도 반환하지 않는다). 즉 이 절차로는 CrashLoopBackOff 를 볼 수 없다.
- `fix-renew` 로 복구하면 **재획득자가 이전과 같은 Pod 일 수 있다.** 훼손 중에는 반납도 거부돼
  Lease 홀더가 옛 리더로 남아 있고, 옛 리더는 자기 identity 가 홀더라 `LeaseDuration` 만료를
  기다리지 않고 바로 갱신할 수 있다. 팔로워가 이길 수도 있는 레이스다.

### 7. OnStoppedLeading 은 리더가 아니어도 호출된다 - 설명만

client-go 콜백 문서가 명시한다: "OnStoppedLeading ... is always called when the LeaderElector
exits, even if it did not start leading. Users should not assume that OnStoppedLeading is only
called after OnStartedLeading." 즉 리더가 된 적 없는 팔로워가 종료해도 이 콜백이 불린다
(`Run` 안 defer). 따라서 **OnStoppedLeading 의 정리 로직은 멱등이어야 하고, OnStartedLeading 이
먼저 실행됐다고 가정하면 안 된다**(예: OnStartedLeading 에서 연 핸들을 닫는다면 nil 여부를 먼저
확인). 이 순서 보장은 래퍼가 대신 만들어 줄 수 없다 - OnStartedLeading/OnNewLeader 는 client-go
가 별도 goroutine 으로 띄우고 OnStoppedLeading 만 메인 goroutine defer 라, "리더였는지" 를 race
없이 알려주는 동기 신호가 없기 때문이다. 그래서 이 스니펫은 client-go 계약을 그대로 노출한다.

### 8. renew 루프가 멈춘 것은 프로세스 생존으로 알 수 없다 - 미노출

리더가 갱신에 실패하면 client-go 가 `Run` 을 반환하므로 이 스니펫은 그 신호를 `ErrLostLease` 로
다룬다(함정 6). 하지만 renew 루프 **자체가 멈춰 버린** 경우(deadlock, goroutine 누수 등)에는
반환도 없고 프로세스도 살아 있어 kubelet 이 보기엔 멀쩡하다. 리더 자리를 쥔 채 아무 일도 하지
않는 최악의 상태가 조용히 유지된다.

client-go 는 이를 위해 `LeaderElectionConfig.WatchDog`(`leaderelection.NewLeaderHealthzAdaptor`)
을 제공한다. 마지막 갱신 시각이 기준을 넘으면 `Check` 가 실패하므로, healthz 핸들러에 물려
liveness probe 로 Pod 를 재시작시킬 수 있다. 이 스니펫은 선출 생명주기 자체에 집중하려고
`Config` 에 노출하지 않았다 - 실전 배포라면 고려할 것.

## 테스트 전략

`go test -race -count=10` 을 통과하는 결정적 테스트 13개. 클러스터 없이 **선출 루프를 실제로
돌린다.**

**fake clientset 의 reactor 로 renew 실패를 만든다.** 이 스니펫에서 제일 쓸모 있는 기법이다.
`k8s.io/client-go/kubernetes/fake` 는 Lease 를 get/create/update 하는 경로를 그대로 태워 주고,
`PrependReactor` 로 그중 `update` 만 가로채면 **ctx 는 살려둔 채 갱신만 실패**시킬 수 있다.

```go
c.PrependReactor("update", "leases", func(k8stesting.Action) (bool, runtime.Object, error) {
    if b.fail.Load() {
        return true, nil, apierrors.NewConflict(...) // 갱신 실패 -> 비자발적 상실
    }
    return false, nil, nil // 기본 트래커로 넘긴다
})
```

`make break-renew` 가 Role 에서 `update` verb 만 회수하는 것과 정확히 같은 원리다. renew 는 Lease
`update` 이므로, API 파티션이나 시계 조작 없이 "리더가 임대를 놓치는" 상황을 만들 수 있다.
클러스터 실험과 단위 테스트가 같은 지렛대를 쓴다.

**상실 확정은 `OnStoppedLeading` 으로 붙잡는다.** `time.Sleep(600ms)` 으로 "이쯤이면 상실됐겠지"
하고 넘기면 느린 머신에서 아직 리더인 채로 갱신을 다시 허용해, 재획득이 일어나지 않고 테스트가
타임아웃으로 죽는다. client-go 는 이 콜백을 `LeaderElector.Run` 의 `defer` 로 부르므로 **불렸다는
것 자체가 "renew 가 이미 포기했다" 는 확정 신호**다. 판정에 영향을 주는 sleep 은 쓰지 않는다.

**예외인 sleep 하나.** `WaitForLeaderWork` 테스트의 콜백 안에는 정리 지연을 흉내 내는
`time.Sleep(400ms)` 이 있다. 이건 남겨도 된다 - 느린 머신에서는 겹침 창이 **넓어지므로** 거짓
통과가 아니라 거짓 실패 방향이고, 깨진 구현이 오히려 더 잘 잡힌다.

**타이밍 값은 관계식이 허락하는 최소로.** 헬퍼의 `fastConfig` 는 300ms/200ms/50ms 를 쓴다.
기본값(15s/10s/2s)으로는 테스트가 못 돈다. `RetryPeriod * JitterFactor(1.2) < RenewDeadline <
LeaseDuration` 을 지키는 선에서 최대한 줄인 값이다(함정 2).

**역방향은 단언하지 않는다.** "`WaitForLeaderWork` 를 끄면 겹침이 **발생한다**" 는 단언하지
않는다. 재획득 속도에 따라 안 겹칠 수도 있는 타이밍 의존 현상이라 flaky 해진다. 이 옵션은
"켜면 동시 실행이 1 을 넘지 않는다" 한 방향만 고정한다.

**계약도 테스트로 고정한다.** 함정 7(`OnStoppedLeading` 은 리더가 된 적 없어도 불린다)은 미리
취소한 ctx 로 `Run` 을 불러 `OnStartedLeading` 0 회 / `OnStoppedLeading` 1 회로 확인한다. 함정 3
(`ReleaseOnCancel` 반납)은 취소 후 `holderIdentity` 가 빈 문자열이 되는지로 확인한다. 타이밍
관계식 검증을 client-go 에 위임한 것(함정 2)도 `Run` 이 `NewLeaderElector` 의 에러를 그대로
돌려주는지로 고정한다.

**테스트로 고정하지 않는 것 하나.** `Run` 이 종료 사유를 `WaitForLeaderWork` 대기 **전에**
확정한다는 순서는 테스트가 없다. 사유를 캡처하는 시점과 상위 ctx 취소 사이에 관측 가능한
happens-before 엣지가 없어서(래퍼가 내부 상태를 노출하지 않는다) 어떤 테스트를 짜도 결국
타이밍 마진에 기대게 되고, 그 마진이 어긋나면 **거짓 실패** 방향으로 깨진다. 그래서 이 순서는
코드 배치와 그 자리의 주석으로만 보장한다.

## 참고 자료

- [client-go leaderelection 패키지](https://pkg.go.dev/k8s.io/client-go/tools/leaderelection)
- [Lease API (coordination.k8s.io)](https://kubernetes.io/docs/concepts/architecture/leases/)
- [Simple leader election with Kubernetes](https://kubernetes.io/blog/2016/01/simple-leader-election-with-kubernetes/)
- [Downward API 로 Pod 정보 노출](https://kubernetes.io/docs/tasks/inject-data-application/environment-variable-expose-pod-information/)

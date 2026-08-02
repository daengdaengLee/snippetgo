# k8s Lease 기반 리더 선출 스니펫 - 로컬 kind 데모용 Makefile.
#
# 전형적 흐름: make demo (정리(클러스터/로컬 이미지) -> 클러스터 구축 -> 이미지 빌드/적재 -> 배포)
# 그다음 make logs 로 한 Pod 만 리더가 되는 것을 관찰한다.

CLUSTER ?= snippetgo-le
# 이 Makefile 은 kind 전용이다. current-context 를 따라가면 컨텍스트를 바꾼 뒤 부른
# break-renew 가 엉뚱한 클러스터의 RBAC 를 훼손하고 kill-leader 가 남의 Pod 를 지운다.
# kind 는 컨텍스트를 kind-<name> 으로 만드므로 거기에 고정한다. 클러스터가 없을 때도
# "connection refused" 대신 "context does not exist" 로 원인이 분명해진다.
KUBECTL := kubectl --context kind-$(CLUSTER)
# 배포할 kustomize 디렉터리. base 는 exit, overlays/rejoin 은 rejoin 모드.
KUSTOMIZE_DIR ?= manifests/base
# 아래 세 값은 manifests/ 의 값을 미러링하는 상수다(매니페스트가 source of truth).
# knob 이 아니므로 override 로 커맨드라인 변수까지 막는다 - := 만으로는 환경변수 상속만
# 막히고 make IMAGE=... load 는 그대로 먹는다. 특히 IMAGE 가 바뀌면 빌드/적재 태그만
# 바뀌고 배포 매니페스트는 그대로라 롤아웃이 90s 타임아웃으로 실패한다(ErrImageNeverPull).
override NAMESPACE  := snippetgo-leaderelection
override LEASE_NAME := snippetgo-leaderelection
override IMAGE      := snippetgo/leaderelection:dev
# 바이너리는 kind 노드(리눅스) 에서 돌므로 GOOS 는 linux 로 고정하고
# GOARCH 는 호스트에 맞춰 kind 노드 아키텍처와 일치시킨다.
GOARCH ?= $(shell go env GOARCH)

.DEFAULT_GOAL := help

# demo 의 전제 조건은 순서가 곧 정확성이다(정리 -> 클러스터 생성 -> 배포).
# make -j 로 부르면 전제 조건이 병렬로 떠서 clean(-> kind-down) 이 kind-up 이 만들고 있는
# 클러스터를 지우거나, kind create 가 "already exist" 로 실패한다. 이 순서는 의존 그래프로
# 표현할 수 없으므로(kind-up 은 클러스터가 이미 있으면 실패해서 load 의 전제조건이 될 수 없다)
# 목록 순서로만 보장된다. 그래서 이 Makefile 은 병렬 실행을 금지한다.
.NOTPARALLEL:

.PHONY: help
help: ## 사용 가능한 타깃 목록
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## 정적 리눅스 바이너리를 bin/leaderelection 로 빌드
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -o bin/leaderelection ./cmd

# --platform 을 주는 이유: 빌드한 바이너리(GOARCH)와 이미지 플랫폼을 맞춘다.
# DOCKER_DEFAULT_PLATFORM 이 다르게 잡혀 있어도 exec format error 를 피한다.
.PHONY: docker-build
docker-build: build ## 바이너리를 복사한 컨테이너 이미지 빌드
	docker build --platform=linux/$(GOARCH) -t $(IMAGE) .

.PHONY: kind-up
kind-up: ## kind 클러스터 생성
	kind create cluster --name $(CLUSTER) --config kind-config.yaml

.PHONY: kind-down
kind-down: ## kind 클러스터 삭제(없어도 무시)
	-kind delete cluster --name $(CLUSTER)

.PHONY: load
load: docker-build ## 이미지 빌드 후 kind 노드에 적재
	kind load docker-image $(IMAGE) --name $(CLUSTER)

# rollout restart 가 필요한 이유: 태그가 :dev 로 고정이라 apply 만으로는 새 이미지가
# 반영되지 않는다(spec 변화가 없어 롤아웃이 안 난다). 이미 돌고 있는 Pod 를 방금 적재한
# 이미지로 갈아치운다.
.PHONY: deploy
deploy: load ## 빌드/적재 -> apply -> Pod 재시작 -> 롤아웃 대기(kind 클러스터 필수)
	$(KUBECTL) apply -k $(KUSTOMIZE_DIR)
	$(KUBECTL) -n $(NAMESPACE) rollout restart deploy/leaderelection
	$(KUBECTL) -n $(NAMESPACE) rollout status deploy/leaderelection --timeout=90s

.PHONY: deploy-rejoin
deploy-rejoin: ## 클러스터 재생성 없이 rejoin overlay 재배포(빌드/적재/Pod 재시작 포함)
	$(MAKE) deploy KUSTOMIZE_DIR=manifests/overlays/rejoin

.PHONY: status
status: ## Lease 와 Pod 상태 확인
	$(KUBECTL) -n $(NAMESPACE) get lease,pods -o wide

.PHONY: logs
logs: ## 모든 Pod 로그를 실시간으로 따라가기(리더 관찰)
	$(KUBECTL) -n $(NAMESPACE) logs -l app=leaderelection -f --prefix --max-log-requests=10

.PHONY: leader
leader: ## 현재 리더(Lease holderIdentity) 출력
	$(KUBECTL) -n $(NAMESPACE) get lease $(LEASE_NAME) -o jsonpath='{.spec.holderIdentity}{"\n"}'

# identity == Pod 이름(Downward API metadata.name) 이라는 전제에 의존한다.
# cmd/main.go 의 identity 규칙을 바꾸면 이 타깃도 같이 고쳐야 한다.
.PHONY: kill-leader
kill-leader: ## 현재 리더 Pod 를 삭제해 failover 유도
	@leader=$$($(KUBECTL) -n $(NAMESPACE) get lease $(LEASE_NAME) -o jsonpath='{.spec.holderIdentity}'); \
	if [ -z "$$leader" ]; then echo "현재 리더 없음(선출 전이거나 홀더 비어 있음)"; exit 1; fi; \
	echo "리더 Pod 삭제: $$leader"; \
	$(KUBECTL) -n $(NAMESPACE) delete pod "$$leader"

# renew 는 Lease update 이므로 update 만 막으면 ctx 는 살아 있는 채 갱신만 실패한다
# = API 파티션 없이 비자발적 상실을 재현.
#
# update 를 가진 규칙은 rules[0] 이다(rules[1] 은 create 전용). JSON Patch 는 인덱스로만
# 짚을 수 있어, rbac.yaml 의 규칙 순서가 바뀌면 엉뚱한 규칙을 조용히 훼손할 수 있다.
# 앞의 test op 가 그 침묵 실패를 소리나는 실패로 바꾼다 - patch 는 원자적이라 test 가
# 어긋나면 replace 도 적용되지 않는다. 대가는 두 번 부르면 실패한다는 것이고, 그 경우도
# 아래 안내가 원인을 짚어 준다.
.PHONY: break-renew
break-renew: ## Role 에서 update 권한을 회수해 비자발적 리더직 상실 유도
	@$(KUBECTL) -n $(NAMESPACE) patch role leaderelection --type=json \
		-p='[{"op":"test","path":"/rules/0/verbs","value":["get","update"]},{"op":"replace","path":"/rules/0/verbs","value":["get"]}]' \
		|| { echo "patch 실패: 이미 훼손됐거나(-> make fix-renew) rbac.yaml 의 규칙 순서가 바뀌었다"; exit 1; }
	@echo "Role 이 훼손된 상태다. 복구: make fix-renew (Deployment 를 건드리지 않아 exit/rejoin 모드는 유지된다)"

# 매니페스트가 source of truth 다. verb 목록을 여기에 다시 적으면 rbac.yaml 이 바뀔 때
# 조용히 어긋난다. apply 는 3-way merge 로 훼손된 live 값을 매니페스트 값으로 되돌리고,
# apply -k 와 달리 Deployment 를 건드리지 않아 LE_MODE(exit/rejoin) 가 그대로 남는다.
.PHONY: fix-renew
fix-renew: ## rbac.yaml 을 재적용해 update 권한을 복구한다
	$(KUBECTL) apply -f manifests/base/rbac.yaml
	@echo "RBAC 복구 완료(rbac.yaml 기준)"

# demo 의 전제조건은 clean kind-up deploy 로 충분하다. deploy: load, load: docker-build,
# docker-build: build 가 걸려 있어 빌드/적재는 deploy 가 끌고 온다. kind-up 만 목록에
# 남기는 이유는 그것만 의존 그래프로 표현할 수 없기 때문이다(위 .NOTPARALLEL 주석 참고).
.PHONY: demo
demo: clean kind-up deploy ## 정리(클러스터/이미지) -> 구축 -> 빌드/적재 -> 배포(base=exit)
	@echo ""
	@echo "배포 완료. 리더를 관찰하려면: make logs"
	@echo "현재 리더 확인:        make leader"
	@echo "failover 실험:         make kill-leader"
	@echo "비자발적 상실 실험:    make break-renew (복구: make fix-renew)"

.PHONY: demo-rejoin
demo-rejoin: ## demo 를 rejoin overlay 로 실행(리더직 상실 시 in-process 재경쟁)
	$(MAKE) demo KUSTOMIZE_DIR=manifests/overlays/rejoin

# 이미지도 남기지 않는다. docker-build 가 매번 재태깅하므로 demo 가 쓰는 이미지가
# 바뀌는 건 아니고, 디스크 위생과 "빌드 실패 시 옛 이미지가 남지 않는다" 가 목적이다.
# 이미지 삭제는 조용히 실패해야 한다 - 이미지가 없는 첫 실행이 정상 경로다.
.PHONY: clean
clean: kind-down ## 클러스터, 로컬 이미지, 빌드 산출물 정리
	@docker image rm $(IMAGE) 2>/dev/null || true
	rm -rf bin

# k8s Lease 기반 리더 선출 스니펫 - 로컬 kind 데모용 Makefile.
#
# 전형적 흐름: make demo (정리 -> 클러스터 구축 -> 이미지 빌드/적재 -> 배포)
# 그다음 make logs 로 한 Pod 만 리더가 되는 것을 관찰한다.

IMAGE     ?= snippetgo/leaderelection:dev
CLUSTER   ?= snippetgo-le
# NAMESPACE/LEASE_NAME 은 manifests/ 에 하드코딩된 값과 반드시 일치해야 한다
# (매니페스트가 source of truth). 이 변수만 바꿔도 배포 위치는 옮겨지지 않는다.
NAMESPACE  ?= snippetgo-leaderelection
LEASE_NAME ?= snippetgo-leaderelection
# 리더직 상실 처리 방식(exit | rejoin). deploy 가 Deployment 에 주입한다.
MODE       ?= exit
# 바이너리는 kind 노드(리눅스) 에서 돌므로 GOOS 는 linux 로 고정하고
# GOARCH 는 호스트에 맞춰 kind 노드 아키텍처와 일치시킨다.
GOARCH    ?= $(shell go env GOARCH)

.DEFAULT_GOAL := help

.PHONY: help
help: ## 사용 가능한 타깃 목록
	@grep -E '^[a-zA-Z_-]+:.*?## .*$$' $(MAKEFILE_LIST) | \
		awk 'BEGIN {FS = ":.*?## "}; {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}'

.PHONY: build
build: ## 정적 리눅스 바이너리를 bin/leaderelection 로 빌드
	CGO_ENABLED=0 GOOS=linux GOARCH=$(GOARCH) go build -o bin/leaderelection ./cmd

.PHONY: docker-build
docker-build: build ## 바이너리를 복사한 컨테이너 이미지 빌드
	docker build -t $(IMAGE) .

.PHONY: kind-up
kind-up: ## kind 클러스터 생성
	kind create cluster --name $(CLUSTER) --config kind-config.yaml

.PHONY: kind-down
kind-down: ## kind 클러스터 삭제(없어도 무시)
	-kind delete cluster --name $(CLUSTER)

.PHONY: load
load: ## 빌드한 이미지를 kind 노드에 적재
	kind load docker-image $(IMAGE) --name $(CLUSTER)

.PHONY: deploy
deploy: ## 네임스페이스/RBAC/Deployment(MODE 렌더) 적용 후 롤아웃 대기
	kubectl apply -f manifests/namespace.yaml -f manifests/rbac.yaml
	# deployment.yaml 을 단일 source of truth 로 두고 MODE 만 렌더해 한 번만 적용한다
	# (value: exit 은 LE_MODE 에만 있는 유일 문자열. MODE=exit 이면 no-op 치환).
	sed 's/value: exit/value: $(MODE)/' manifests/deployment.yaml | kubectl apply -f -
	kubectl -n $(NAMESPACE) rollout status deploy/leaderelection --timeout=90s

.PHONY: status
status: ## Lease 와 Pod 상태 확인
	kubectl -n $(NAMESPACE) get lease,pods -o wide

.PHONY: logs
logs: ## 모든 Pod 로그를 실시간으로 따라가기(리더 관찰)
	kubectl -n $(NAMESPACE) logs -l app=leaderelection -f --prefix --max-log-requests=10

.PHONY: leader
leader: ## 현재 리더(Lease holderIdentity) 출력
	kubectl -n $(NAMESPACE) get lease $(LEASE_NAME) -o jsonpath='{.spec.holderIdentity}{"\n"}'

.PHONY: kill-leader
kill-leader: ## 현재 리더 Pod 를 삭제해 failover 유도
	@leader=$$(kubectl -n $(NAMESPACE) get lease $(LEASE_NAME) -o jsonpath='{.spec.holderIdentity}'); \
	if [ -z "$$leader" ]; then echo "현재 리더 없음(선출 전이거나 홀더 비어 있음)"; exit 1; fi; \
	echo "리더 Pod 삭제: $$leader"; \
	kubectl -n $(NAMESPACE) delete pod "$$leader"

.PHONY: demo
demo: kind-down kind-up docker-build load deploy ## 정리 -> 구축 -> 빌드/적재 -> 배포(MODE=exit 기본)
	@echo ""
	@echo "배포 완료(mode=$(MODE)). 리더를 관찰하려면: make logs"
	@echo "현재 리더 확인:        make leader"
	@echo "failover 실험:         make kill-leader"

.PHONY: demo-rejoin
demo-rejoin: ## demo 를 rejoin 모드로 실행(리더직 상실 시 in-process 재경쟁)
	$(MAKE) demo MODE=rejoin

.PHONY: clean
clean: kind-down ## 클러스터와 빌드 산출물 정리
	rm -rf bin

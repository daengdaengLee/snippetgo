# 로컬에서 make build 로 만든 정적 바이너리(bin/leaderelection)를 복사만 한다.
# 빌드 자체는 Makefile 이 담당하고, 이미지는 최소 런타임만 제공한다.
FROM gcr.io/distroless/static:nonroot

COPY bin/leaderelection /leaderelection

# distroless nonroot 는 uid 65532 로 실행된다. Deployment 의 securityContext 와 정합.
ENTRYPOINT ["/leaderelection"]

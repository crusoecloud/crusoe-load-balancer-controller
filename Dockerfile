# The shared Go pipeline supplies the platform binary; local image tasks do too.
FROM gcr.io/distroless/static:nonroot
ARG TARGETOS=linux
ARG TARGETARCH
WORKDIR /
COPY bin/${TARGETOS}-${TARGETARCH}/cmd /manager
USER 65532:65532
ENTRYPOINT ["/manager"]

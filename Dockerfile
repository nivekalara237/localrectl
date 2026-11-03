FROM alpine:3.24

ARG TARGETPLATFORM

RUN apk add --no-cache ca-certificates tzdata \
 && addgroup -S -g 10001 localrectl \
 && adduser -S -D -H -u 10001 -G localrectl localrectl

COPY ${TARGETPLATFORM}/localrectl /usr/local/bin/localrectl
COPY LICENSE NOTICE /usr/share/licenses/localrectl/

USER localrectl:localrectl

ENTRYPOINT ["/usr/local/bin/localrectl"]
CMD ["--help"]

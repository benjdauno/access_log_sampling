FROM alpine:latest AS prep
RUN apk --update add ca-certificates

FROM scratch

ARG USER_UID=10001
ARG USER_GID=10001
USER ${USER_UID}:${USER_GID}

COPY --from=prep /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY bin/affirm-otelcol /
# This is a temporary solution until we have the slo.yaml file in an s3 bucket 
# that we pull initially and poll periodically for updates. 
# This mounting should be removed when that is available.
COPY dev_tooling/slo.yaml /
EXPOSE 4317 55680 55679 8888
ENTRYPOINT ["/affirm-otelcol"]
CMD ["--config", "/etc/otel/config.yaml"]

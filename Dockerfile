FROM alpine:latest

RUN apk --update add \
  ca-certificates \
  && rm -rf /var/cache/apk/* \
  && update-ca-certificates

ADD sql_exporter /usr/local/bin/sql_exporter
ENTRYPOINT [ "/usr/local/bin/sql_exporter" ]
EXPOSE 8080

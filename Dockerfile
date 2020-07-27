FROM quay.io/prometheus/golang-builder as builder

ADD .   /go/src/github.com/lukas-mi/sql_exporter-1
WORKDIR /go/src/github.com/lukas-mi/sql_exporter-1

RUN make

FROM        quay.io/prometheus/busybox:glibc
MAINTAINER  The Prometheus Authors <prometheus-developers@googlegroups.com>

COPY --from=builder /go/src/github.com/lukas-mi/sql_exporter-1/sql_exporter-1  /bin/sql_exporter

EXPOSE      9237
ENTRYPOINT  [ "/bin/sql_exporter" ]

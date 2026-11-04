FROM golang:1.25-alpine@sha256:1ae0735f00daffa3aaf1363a5184c0d2dc55c78e3db4ec70241cdac97bf84b59 AS build
COPY main.go /src/
RUN cd /src && CGO_ENABLED=0 go build -trimpath -o /lockd main.go

FROM busybox:1.36@sha256:73aaf090f3d85aa34ee199857f03fa3a95c8ede2ffd4cc2cdb5b94e566b11662
COPY --from=build /lockd /usr/local/bin/lockd
ENTRYPOINT ["lockd"]

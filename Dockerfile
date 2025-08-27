FROM golang:1.24-alpine AS gobgpd_builder

WORKDIR /gobgp_app

COPY ./gobgp_src .

RUN go mod vendor
RUN go build -mod=vendor -o gobgpd ./cmd/gobgpd
RUN go build -mod=vendor -o gobgp ./cmd/gobgp

FROM golang:1.24-alpine AS reconciler_builder

WORKDIR /k8gobgp_app

COPY . .

RUN go build -mod=vendor -o manager ./cmd/manager

FROM alpine/git

WORKDIR /

COPY --from=gobgpd_builder /gobgp_app/gobgpd /usr/local/bin/gobgpd
COPY --from=gobgpd_builder /gobgp_app/gobgp /usr/local/bin/gobgp

COPY --from=reconciler_builder /k8gobgp_app/manager /usr/local/bin/manager

EXPOSE 50051

COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
CMD []
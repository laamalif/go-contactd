FROM golang:1.24-alpine AS build

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS="${TARGETOS:-linux}" GOARCH="${TARGETARCH:-$(go env GOARCH)}" \
	go build -trimpath -ldflags='-s -w' -o /out/contactd ./cmd/contactd && \
	ln -sf /contactd /out/contactctl && \
	ln -sf /contactd /out/go-contactd

FROM gcr.io/distroless/static:nonroot

ENV PORT=8080 \
	CONTACTD_DB_PATH=/data/contactd.sqlite \
	CONTACTD_LOG_FORMAT=text

COPY --from=build /out/contactd /contactd
COPY --from=build /out/contactctl /contactctl
COPY --from=build /out/go-contactd /go-contactd

VOLUME ["/data"]
EXPOSE 8080

ENTRYPOINT ["/contactd"]

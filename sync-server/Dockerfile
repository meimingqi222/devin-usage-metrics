FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /sync-api .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /sync-api /sync-api
ENV SYNC_LISTEN=:8080
VOLUME ["/data"]
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/sync-api"]

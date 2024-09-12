export GOARCH="${GOARCH:-amd64}"

docker build -f package/Dockerfile -t frankyang/fleet:dev --build-arg="ARCH=$GOARCH"  .
docker build -f package/Dockerfile.agent -t frankyang/fleet-agent:dev --build-arg="ARCH=$GOARCH" .

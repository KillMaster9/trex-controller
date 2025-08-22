#!/bin/bash

LOG_DIR="/var/log/trex"
# 启动 trex-controller 服务
docker run -d --name trex-controller --network host \
  --cap-add=NET_ADMIN \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v $LOG_DIR:/var/log
  trex-controller

echo "TREx Controller deployed. Use bin/trexctl to manage containers."
echo "Logs are stored in $LOG_DIR/trex-controller.log"
echo "Use 'docker logs trex-controller' to view console logs"

docker run -it --network=host --cap-add=ALL \
  -v /var/run/docker.sock:/var/run/docker.sock \
  -v /var/log/trex:/var/log  -v /tmp/trex:/tmp/trex \
  registry.cn-beijing.aliyuncs.com/killmaster/trex-controller:trex-controller
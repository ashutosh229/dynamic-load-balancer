#!/bin/bash
cd /opt/lb
while true; do
  ./lb -config config.json
  echo "lb crashed, restarting in 2s..." >> /opt/lb/lb.log
  sleep 2
done

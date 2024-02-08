# IBM Cloud Code Engine - Metrics Collector

Code Engine job that demonstrates how to collect resource metrics (CPU and memory) of running Code Engine apps, jobs, and builds

### Installation

* Create Code Engine job template
```
$ ibmcloud ce job create \
    --name metrics-collector \
    --src . \
    --mode daemon \
    --cpu 0.25 \
    --memory 0.5G
```

* Submit a daemon job that collects metrics in an endless loop. The daemon job queries the Metrics API every 10 seconds
```
$ ibmcloud ce jobrun submit \
    --job metrics-collector \
    --env INTERVAL=10 
```
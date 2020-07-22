package main

import (
	"flag"
	"github.com/NVIDIA/gpu-monitoring-tools/bindings/go/nvml"
	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/ec2metadata"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {

	idleMemoryUsage := flag.Uint64("idle", 0, "idle memory max limit")
	usageInteval := flag.Int("usage-interval", 100, "process usage checker interval in ms")
	publishMetricInterval := flag.Int("metric-interval", 1, "process usage checker interval in min")

	flag.Parse()

	nvml.Init()
	defer nvml.Shutdown()

	deviceCount, err := nvml.GetDeviceCount()
	if err != nil {
		panic(err)
	}

	log.Printf("Device count %v", deviceCount)

	var devices []*nvml.Device
	for i := uint(0); i < deviceCount; i++ {
		device, err := nvml.NewDevice(i)
		if err != nil {
			log.Panicf("Error getting device %d: %v\n", i, err)
		}
		devices = append(devices, device)
	}

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	usageTicker := time.NewTicker(time.Millisecond * time.Duration(*usageInteval))
	defer usageTicker.Stop()

	metricTicker := time.NewTicker(time.Minute * time.Duration(*publishMetricInterval))
	defer metricTicker.Stop()

	gpuMonitor := New(*idleMemoryUsage)
	gpuMonitor.init()

	for {
		select {
		case <-usageTicker.C:
			percentage, err := gpuMonitor.getUsagePercentage(devices)
			if err != nil {
				log.Printf("runcommand err : %v", err)
				continue
			}
			gpuMonitor.updateValues(percentage)
		case <-metricTicker.C:
			gpuMonitor.sendMetric()
			gpuMonitor.reset()
		case <-sigs:
			return
		}
	}
}

func (gpu *GPUMonitor) getUsagePercentage(devices []*nvml.Device) (float64, error) {
	processCount := 0
	idleProcessCount := 0
	for _, device := range devices {
		pInfo, err := device.GetAllRunningProcesses()
		if err != nil {
			return 0.0, err
		}
		for j := range pInfo {
			processInfo := pInfo[j]
			if processInfo.MemoryUsed <= gpu.idleMemoryUsage {
				idleProcessCount++
			}
			processCount++
		}
	}
	return float64(processCount-idleProcessCount) / float64(processCount), nil
}

type GPUMonitor struct {
	svc             *cloudwatch.CloudWatch
	instanceID      string
	idleMemoryUsage uint64

	minPercentage   float64
	maxPercentage   float64
	counter         int64
	totalPercentage float64
}

func New(idleMemoryUsage uint64) *GPUMonitor {
	return &GPUMonitor{
		instanceID:      "not-found",
		idleMemoryUsage: idleMemoryUsage,
	}
}

func (w *GPUMonitor) reset() {
	w.minPercentage = 100.0
	w.maxPercentage = 0.0
	w.counter = 0
	w.totalPercentage = 0
}

func (w *GPUMonitor) setInstanceIdFromMetadata() {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))

	metadataSvc := ec2metadata.New(sess)

	if metadataSvc.Available() {
		metadata, err := metadataSvc.GetMetadata("instance-id")
		if err != nil {
			log.Printf("Error while taken instance-id from metadat %v", err)
		} else {
			w.instanceID = metadata
		}
	}
}

func (w *GPUMonitor) sendMetric() {
	statisticValues := &cloudwatch.StatisticSet{
		Maximum:     aws.Float64(w.maxPercentage),
		Minimum:     aws.Float64(w.minPercentage),
		SampleCount: aws.Float64(float64(w.counter)),
		Sum:         aws.Float64(w.totalPercentage),
	}

	out, err := w.svc.PutMetricData(&cloudwatch.PutMetricDataInput{
		Namespace: aws.String("GPUMemoryUsage"),
		MetricData: []*cloudwatch.MetricDatum{
			{
				MetricName:      aws.String("RequestCountPerInstance"),
				Unit:            aws.String("Percent"),
				StatisticValues: statisticValues,
				Timestamp:       aws.Time(time.Now()),
				Dimensions: []*cloudwatch.Dimension{
					{
						Name:  aws.String("Instance-Id"),
						Value: aws.String(w.instanceID),
					},
				},
			},
			{
				MetricName:      aws.String("AcrossAllInstances"),
				Unit:            aws.String("Percent"),
				StatisticValues: statisticValues,
				Timestamp:       aws.Time(time.Now()),
			},
		},
	})

	if err != nil {
		log.Printf("put metric error : %v", err)
	} else {
		log.Printf("metric put successfly %v", out)
	}
}

func (w *GPUMonitor) updateValues(percentage float64) {
	if percentage < w.minPercentage {
		w.minPercentage = percentage
	}
	if w.maxPercentage < percentage {
		w.maxPercentage = percentage
	}
	w.totalPercentage += percentage
	w.counter++
}

func (w *GPUMonitor) init() {
	sess := session.Must(session.NewSessionWithOptions(session.Options{
		SharedConfigState: session.SharedConfigEnable,
	}))
	// Create the cloudwatch events client
	svc := cloudwatch.New(sess)
	w.svc = svc

	w.setInstanceIdFromMetadata()
	w.reset()
}

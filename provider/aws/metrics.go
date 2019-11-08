package aws

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/service/cloudformation"
	"github.com/aws/aws-sdk-go/service/cloudwatch"
	"github.com/convox/rack/pkg/options"
	"github.com/convox/rack/pkg/structs"
)

func (p *Provider) cloudwatchMetrics(mdqs []metricDataQuerier, opts structs.MetricsOptions) (structs.Metrics, error) {
	period := ci(opts.Period, 3600)

	req := &cloudwatch.GetMetricDataInput{
		EndTime:           aws.Time(ct(opts.End, time.Now())),
		MetricDataQueries: []*cloudwatch.MetricDataQuery{},
		ScanBy:            aws.String("TimestampAscending"),
		StartTime:         aws.Time(ct(opts.Start, time.Now().Add(-24*time.Hour))),
	}

	for i, mdq := range mdqs {
		req.MetricDataQueries = append(req.MetricDataQueries, mdq.MetricDataQueries(period, fmt.Sprintf("%d", i))...)
	}

	res, err := p.CloudWatch.GetMetricData(req)
	if err != nil {
		return nil, err
	}

	msh := map[string]map[time.Time]structs.MetricValue{}

	for _, dr := range res.MetricDataResults {
		if dr.Label == nil {
			continue
		}

		parts := strings.SplitN(*dr.Label, "/", 2)

		if len(parts) < 2 {
			continue
		}

		name := parts[0]
		stat := parts[1]

		mvsh, ok := msh[name]
		if !ok {
			mvsh = map[time.Time]structs.MetricValue{}
		}

		for i, ts := range dr.Timestamps {
			if ts == nil {
				continue
			}

			if dr.Values[i] == nil {
				continue
			}

			v := math.Floor(*dr.Values[i]*100) / 100

			mv, ok := mvsh[*ts]
			if !ok {
				mv = structs.MetricValue{Time: *ts}
			}

			switch stat {
			case "Average":
				mv.Average += v
			case "Minimum":
				mv.Minimum += v
			case "Maximum":
				mv.Maximum += v
			case "P90":
				mv.P90 += v
			case "P95":
				mv.P95 += v
			case "P99":
				mv.P99 += v
			case "SampleCount":
				mv.Count += v / (float64(period) / 60)
			case "Sum":
				mv.Sum += v
			}

			mvsh[*ts] = mv
		}

		msh[name] = mvsh
	}

	ms := structs.Metrics{}

	for name, mvsh := range msh {
		m := structs.Metric{Name: name}

		mvs := structs.MetricValues{}

		for _, mv := range mvsh {
			mvs = append(mvs, mv)
		}

		sort.Slice(mvs, func(i, j int) bool { return mvs[i].Time.Before(mvs[j].Time) })

		m.Values = mvs

		ms = append(ms, m)
	}

	sort.Slice(ms, func(i, j int) bool { return ms[i].Name < ms[j].Name })

	return ms, nil
}

func (p *Provider) appMetricDefinitions(app string) ([]metricDataQuerier, error) {
	rs, err := p.describeStackResources(&cloudformation.DescribeStackResourcesInput{
		StackName: aws.String(p.rackStack(app)),
	})
	if err != nil {
		return nil, err
	}

	sr, err := p.describeStack(p.Rack)
	if err != nil {
		return nil, err
	}

	ros := stackOutputs(sr)

	mdqs := []metricDataQuerier{}

	for _, r := range rs.StackResources {
		if r.ResourceType != nil && r.LogicalResourceId != nil {
			if *r.ResourceType == "AWS::CloudFormation::Stack" && strings.HasPrefix(*r.LogicalResourceId, "Service") {
				s, err := p.describeStack(*r.PhysicalResourceId)
				if err != nil {
					return nil, err
				}

				sos := stackOutputs(s)

				if sv := sos["Service"]; sv != "" {
					svp := strings.Split(sv, "/")
					svn := svp[len(svp)-1]

					mdqs = append(mdqs, metricStatistics{"process:running", "AWS/ECS", "CPUUtilization", map[string]string{"ClusterName": p.Cluster, "ServiceName": svn}, []string{"SampleCount"}})
				}

				if tg := sos["TargetGroup"]; tg != "" {
					tgp := strings.Split(tg, ":")
					tgn := tgp[len(tgp)-1]

					if rn := ros["RouterName"]; rn != "" {
						mdqs = append(mdqs, metricStatistics{"process:healthy", "AWS/ApplicationELB", "HealthyHostCount", map[string]string{"LoadBalancer": rn, "TargetGroup": tgn}, []string{"Average", "Minimum", "Maximum"}})
						mdqs = append(mdqs, metricStatistics{"process:unhealthy", "AWS/ApplicationELB", "UnHealthyHostCount", map[string]string{"LoadBalancer": rn, "TargetGroup": tgn}, []string{"Average", "Minimum", "Maximum"}})
						mdqs = append(mdqs, metricStatistics{"service:requests:2xx", "AWS/ApplicationELB", "HTTPCode_Target_2XX_Count", map[string]string{"LoadBalancer": rn, "TargetGroup": tgn}, []string{"Sum"}})
						mdqs = append(mdqs, metricStatistics{"service:requests:3xx", "AWS/ApplicationELB", "HTTPCode_Target_3XX_Count", map[string]string{"LoadBalancer": rn, "TargetGroup": tgn}, []string{"Sum"}})
						mdqs = append(mdqs, metricStatistics{"service:requests:4xx", "AWS/ApplicationELB", "HTTPCode_Target_4XX_Count", map[string]string{"LoadBalancer": rn, "TargetGroup": tgn}, []string{"Sum"}})
						mdqs = append(mdqs, metricStatistics{"service:requests:5xx", "AWS/ApplicationELB", "HTTPCode_Target_5XX_Count", map[string]string{"LoadBalancer": rn, "TargetGroup": tgn}, []string{"Sum"}})
						// mdqs = append(mdqs, metricStatistics{"service:requests", "AWS/ApplicationELB", "RequestCountPerTarget", map[string]string{"LoadBalancer": rn, "TargetGroup": tgn}, []string{"Sum"}})
						mdqs = append(mdqs, metricStatistics{"service:response:time", "AWS/ApplicationELB", "TargetResponseTime", map[string]string{"LoadBalancer": rn, "TargetGroup": tgn}, []string{"Minimum", "Maximum"}})

						mdqs = append(mdqs, metricExpressions{
							"service:requests",
							"Sum",
							[]metricStatistics{
								metricStatistics{"target_requests", "AWS/ApplicationELB", "RequestCountPerTarget", map[string]string{"LoadBalancer": rn, "TargetGroup": tgn}, []string{"Sum"}},
								metricStatistics{"target_healthy", "AWS/ApplicationELB", "HealthyHostCount", map[string]string{"LoadBalancer": rn, "TargetGroup": tgn}, []string{"Average"}},
							},
							[]metricExpression{metricExpression{"requests", "target_requests_Sum_## * target_healthy_Average_##"}},
						})
					}
				}
			}
		}
	}

	return mdqs, nil
}

func (p *Provider) systemMetricDefinitions() []metricDataQuerier {
	mdqs := []metricDataQuerier{
		metricStatistics{"cluster:cpu:reservation", "AWS/ECS", "CPUReservation", map[string]string{"ClusterName": p.Cluster}, []string{"Average", "Minimum", "Maximum"}},
		metricStatistics{"cluster:cpu:utilization", "AWS/ECS", "CPUUtilization", map[string]string{"ClusterName": p.Cluster}, []string{"Average", "Minimum", "Maximum"}},
		metricStatistics{"cluster:mem:reservation", "AWS/ECS", "MemoryReservation", map[string]string{"ClusterName": p.Cluster}, []string{"Average", "Minimum", "Maximum"}},
		metricStatistics{"cluster:mem:utilization", "AWS/ECS", "MemoryUtilization", map[string]string{"ClusterName": p.Cluster}, []string{"Average", "Minimum", "Maximum"}},
		metricStatistics{"instances:standard:cpu", "AWS/EC2", "CPUUtilization", map[string]string{"AutoScalingGroupName": p.AsgStandard}, []string{"Average", "Minimum", "Maximum"}},
	}

	if p.AsgSpot != "" {
		mdqs = append(mdqs, metricStatistics{"instances:spot:cpu", "AWS/EC2", "CPUUtilization", map[string]string{"AutoScalingGroupName": p.AsgSpot}, []string{"Average", "Minimum", "Maximum"}})
	}

	return mdqs
}

type metricDataQuerier interface {
	MetricDataQueries(period int64, suffix string) []*cloudwatch.MetricDataQuery
}

type metricExpressions struct {
	Name        string
	Statistic   string
	Statistics  []metricStatistics
	Expressions []metricExpression
}

type metricExpression struct {
	Name       string
	Expression string
}

type metricStatistics struct {
	Name       string
	Namespace  string
	Metric     string
	Dimensions map[string]string
	Statistics []string
}

func (me metricExpressions) MetricDataQueries(period int64, suffix string) []*cloudwatch.MetricDataQuery {
	qs := []*cloudwatch.MetricDataQuery{}

	for _, ms := range me.Statistics {
		qs = append(qs, ms.MetricDataQueries(period, suffix)...)
	}

	for _, e := range me.Expressions {
		q := &cloudwatch.MetricDataQuery{
			Id:         aws.String(fmt.Sprintf("%s_%s_%s", strings.ReplaceAll(me.Name, ":", "_"), me.Statistic, suffix)),
			Label:      aws.String(fmt.Sprintf("%s/%s", me.Name, me.Statistic)),
			Expression: aws.String(strings.ReplaceAll(e.Expression, "##", suffix)),
		}

		qs = append(qs, q)
	}

	return qs
}

func (ms metricStatistics) MetricDataQueries(period int64, suffix string) []*cloudwatch.MetricDataQuery {
	qs := []*cloudwatch.MetricDataQuery{}

	dim := []*cloudwatch.Dimension{}
	for k, v := range ms.Dimensions {
		dim = append(dim, &cloudwatch.Dimension{
			Name:  options.String(k),
			Value: options.String(v),
		})
	}

	stats := []*string{}
	for _, s := range ms.Statistics {
		stats = append(stats, aws.String(s))
	}

	for _, s := range ms.Statistics {
		q := &cloudwatch.MetricDataQuery{
			Id:    aws.String(fmt.Sprintf("%s_%s_%s", strings.ReplaceAll(ms.Name, ":", "_"), s, suffix)),
			Label: aws.String(fmt.Sprintf("%s/%s", ms.Name, s)),
			MetricStat: &cloudwatch.MetricStat{
				Metric: &cloudwatch.Metric{
					Dimensions: dim,
					MetricName: aws.String(ms.Metric),
					Namespace:  aws.String(ms.Namespace),
				},
				Period: aws.Int64(period),
				Stat:   aws.String(s),
			},
		}

		qs = append(qs, q)
	}

	return qs
}

package ibm

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/IBM/go-sdk-core/v4/core"
	"github.com/IBM/networking-go-sdk/dnsrecordsv1"
	configv1 "github.com/openshift/api/config/v1"
	"github.com/openshift/cluster-ingress-operator/pkg/dns"
	dnsclient "github.com/openshift/cluster-ingress-operator/pkg/dns/ibm/client"

	iov1 "github.com/openshift/api/operatoringress/v1"
	logf "github.com/openshift/cluster-ingress-operator/pkg/log"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
)

var (
	_                   dns.Provider = &Provider{}
	log                              = logf.Logger.WithName("dns")
	defaultCISRecordTTL              = int64(120)
)

const (
	// CISCustomEndpointName is the key used to identify the CIS service in ServiceEndpoints
	CISCustomEndpointName = "cis"
	defaultCISURL         = "https://api.cis.cloud.ibm.com/"
)

type Provider struct {
	dnsServices map[string]dnsclient.DnsClient
}

// ServiceEndpoint stores the configuration of a custom url to
// override existing defaults of IBM Service API endpoints.
type ServiceEndpoint struct {
	// name is the name of the Power VS service.
	// For example
	// IAM - https://cloud.ibm.com/apidocs/iam-identity-token-api
	Name string
	// url is fully qualified URI with scheme https, that overrides the default generated
	// endpoint for a client.
	URL string
}

// Config is the necessary input to configure the manager.
type Config struct {
	APIKey    string
	CISCRN    string
	UserAgent string
	Zones     []string
	// ServiceEndpoints is the list of Custom API endpoints to use for Provider clients.
	ServiceEndpoints []ServiceEndpoint
}

func NewProvider(config Config) (*Provider, error) {
	if len(config.Zones) < 1 {
		return nil, fmt.Errorf("missing zone data")
	}
	authenticator := &core.IamAuthenticator{
		ApiKey: config.APIKey,
	}
	provider := &Provider{}

	provider.dnsServices = make(map[string]dnsclient.DnsClient)

	for _, zone := range config.Zones {
		options := &dnsrecordsv1.DnsRecordsV1Options{
			Authenticator:  authenticator,
			URL:            getCISEndpointURL(config.ServiceEndpoints),
			Crn:            &config.CISCRN,
			ZoneIdentifier: &zone,
		}

		dnsService, err := dnsrecordsv1.NewDnsRecordsV1(options)
		if err != nil {
			return nil, fmt.Errorf("failed to create a new DNS Service instance: %w", err)
		}
		dnsService.EnableRetries(3, 5*time.Second)
		dnsService.Service.SetUserAgent(config.UserAgent)

		provider.dnsServices[zone] = dnsService
	}

	if err := validateDNSServices(provider); err != nil {
		return nil, fmt.Errorf("failed to validate ibm dns services: %w", err)
	}
	return provider, nil
}

// validateDNSServices validates that provider clients can communicate with
// associated API endpoints by having each client make a get DNS records call.
func validateDNSServices(provider *Provider) error {
	var errs []error
	maxItems := int64(1)
	for _, dnsService := range provider.dnsServices {
		opt := dnsService.NewListAllDnsRecordsOptions()
		opt.PerPage = &maxItems
		if _, _, err := dnsService.ListAllDnsRecords(opt); err != nil {
			errs = append(errs, fmt.Errorf("failed to get dns records: %w", err))
		}
	}
	return kerrors.NewAggregate(errs)
}

func (p *Provider) Ensure(record *iov1.DNSRecord, zone configv1.DNSZone) error {
	return p.createOrUpdateDNSRecord(record, zone)
}

func (p *Provider) Replace(record *iov1.DNSRecord, zone configv1.DNSZone) error {
	return p.createOrUpdateDNSRecord(record, zone)
}

func (p *Provider) Delete(record *iov1.DNSRecord, zone configv1.DNSZone) error {
	if err := validateInputDNSData(record, zone); err != nil {
		return fmt.Errorf("delete: invalid dns input data: %w", err)
	}
	dnsService, ok := p.dnsServices[zone.ID]
	if !ok {
		return fmt.Errorf("delete: unknown zone: %v", zone.ID)
	}
	opt := dnsService.NewListAllDnsRecordsOptions()
	opt.SetType(string(record.Spec.RecordType))
	opt.SetName(record.Spec.DNSName)
	for _, target := range record.Spec.Targets {
		opt.SetContent(target)
		result, response, err := dnsService.ListAllDnsRecords(opt)
		if err != nil {
			if response != nil && response.StatusCode != http.StatusNotFound {
				return fmt.Errorf("delete: failed to list the dns record: %w", err)
			}
			continue
		}
		if result == nil || result.Result == nil {
			return fmt.Errorf("delete: invalid result")
		}
		for _, resultData := range result.Result {
			if resultData.ID == nil {
				return fmt.Errorf("delete: record id is nil")
			}
			delOpt := dnsService.NewDeleteDnsRecordOptions(*resultData.ID)
			_, delResponse, err := dnsService.DeleteDnsRecord(delOpt)
			if err != nil {
				if delResponse != nil && delResponse.StatusCode != http.StatusNotFound {
					return fmt.Errorf("delete: failed to delete the dns record: %w", err)
				}
			}
			if delResponse != nil && delResponse.StatusCode != http.StatusNotFound {
				log.Info("deleted DNS record", "record", record.Spec, "zone", zone, "target", target)
			}
		}
	}
	return nil
}

func (p *Provider) createOrUpdateDNSRecord(record *iov1.DNSRecord, zone configv1.DNSZone) error {
	if err := validateInputDNSData(record, zone); err != nil {
		return fmt.Errorf("createOrUpdateDNSRecord: invalid dns input data: %w", err)
	}
	dnsService, ok := p.dnsServices[zone.ID]
	if !ok {
		return fmt.Errorf("createOrUpdateDNSRecord: unknown zone: %v", zone.ID)
	}

	// TTL must be between 120 and 2,147,483,647 seconds, or 1 for Automatic.
	if (record.Spec.RecordTTL > 1 && record.Spec.RecordTTL < 120) || record.Spec.RecordTTL == 0 {
		log.Info("Warning: TTL must be between 120 and 2,147,483,647 seconds, or 1 for Automatic. RecordTTL set to default", "default CIS record TTL", defaultCISRecordTTL)
		record.Spec.RecordTTL = defaultCISRecordTTL
	}

	listOpt := dnsService.NewListAllDnsRecordsOptions()
	listOpt.SetType(string(record.Spec.RecordType))
	// Some dns records (e.g. wildcard record) have an ending "." character in the DNSName
	DNSName := strings.TrimSuffix(record.Spec.DNSName, ".")
	listOpt.SetName(DNSName)
	for _, target := range record.Spec.Targets {
		listOpt.SetContent(target)
		result, response, err := dnsService.ListAllDnsRecords(listOpt)
		if err != nil {
			if response != nil && response.StatusCode != http.StatusNotFound {
				return fmt.Errorf("createOrUpdateDNSRecord: failed to list the dns record: %w", err)
			}
			continue
		}
		if result == nil || result.Result == nil {
			return fmt.Errorf("createOrUpdateDNSRecord: invalid result")
		}
		if len(result.Result) == 0 {
			createOpt := dnsService.NewCreateDnsRecordOptions()
			createOpt.SetName(record.Spec.DNSName)
			createOpt.SetType(string(record.Spec.RecordType))
			createOpt.SetContent(target)
			createOpt.SetTTL(record.Spec.RecordTTL)
			_, _, err := dnsService.CreateDnsRecord(createOpt)
			if err != nil {
				return fmt.Errorf("createOrUpdateDNSRecord: failed to create the dns record: %w", err)
			}
			log.Info("created DNS record", "record", record.Spec, "zone", zone, "target", target)
		} else {
			updateOpt := dnsService.NewUpdateDnsRecordOptions(*result.Result[0].ID)
			updateOpt.SetName(record.Spec.DNSName)
			updateOpt.SetType(string(record.Spec.RecordType))
			updateOpt.SetContent(target)
			updateOpt.SetTTL(record.Spec.RecordTTL)
			_, _, err := dnsService.UpdateDnsRecord(updateOpt)
			if err != nil {
				return fmt.Errorf("createOrUpdateDNSRecord: failed to update the dns record: %w", err)
			}
			log.Info("updated DNS record", "record", record.Spec, "zone", zone, "target", target)
		}
	}

	return nil
}

func validateInputDNSData(record *iov1.DNSRecord, zone configv1.DNSZone) error {
	var errs []error
	if record == nil {
		errs = append(errs, fmt.Errorf("validateInputDNSData: dns record is nil"))
	} else {
		if len(record.Spec.DNSName) == 0 {
			errs = append(errs, fmt.Errorf("validateInputDNSData: dns record name is empty"))
		}
		if len(record.Spec.RecordType) == 0 {
			errs = append(errs, fmt.Errorf("validateInputDNSData: dns record type is empty"))
		}
		if len(record.Spec.Targets) == 0 {
			errs = append(errs, fmt.Errorf("validateInputDNSData: dns record content is empty"))
		}
	}
	if len(zone.ID) == 0 {
		errs = append(errs, fmt.Errorf("validateInputDNSData: dns zone id is empty"))
	}
	return kerrors.NewAggregate(errs)

}

// getCISEndpointURL return the IBM CIS url from service endpoins if exist or else returns the default url
func getCISEndpointURL(endpoint []ServiceEndpoint) string {
	for _, ep := range endpoint {
		if ep.Name == CISCustomEndpointName {
			return ep.URL
		}
	}
	return defaultCISURL
}

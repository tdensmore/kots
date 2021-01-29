package types

type StoreAWS struct {
	Region          string `json:"region"`
	AccessKeyID     string `json:"accessKeyID"`
	SecretAccessKey string `json:"secretAccessKey"` // added for unmarshaling, redacted on marshaling
	UseInstanceRole bool   `json:"useInstanceRole"`
}

type StoreGoogle struct {
	JSONFile        string `json:"jsonFile"`
	ServiceAccount  string `json:"serviceAccount"`
	UseInstanceRole bool   `json:"useInstanceRole"`
}

type StoreAzure struct {
	ResourceGroup  string `json:"resourceGroup"`
	StorageAccount string `json:"storageAccount"`
	SubscriptionID string `json:"subscriptionId"`
	TenantID       string `json:"tenantId"`
	ClientID       string `json:"clientId"`
	ClientSecret   string `json:"clientSecret"`
	CloudName      string `json:"cloudName"`
}

type StoreOther struct {
	Region          string `json:"region"`
	AccessKeyID     string `json:"accessKeyID"`
	SecretAccessKey string `json:"secretAccessKey"` // added for unmarshaling, redacted on marshaling
	Endpoint        string `json:"endpoint"`
}

type StoreInternal struct {
	Region               string `json:"region"`
	AccessKeyID          string `json:"accessKeyID"`
	SecretAccessKey      string `json:"secretAccessKey"` // added for unmarshaling, redacted on marshaling
	Endpoint             string `json:"endpoint"`
	ObjectStoreClusterIP string `json:"objectStoreClusterIP"`
}

type StoreNFS struct {
	Region               string `json:"region"`
	AccessKeyID          string `json:"accessKeyID"`
	SecretAccessKey      string `json:"secretAccessKey"` // added for unmarshaling, redacted on marshaling
	Endpoint             string `json:"endpoint"`
	ObjectStoreClusterIP string `json:"objectStoreClusterIP"`
}

type Store struct {
	Provider string         `json:"provider"`
	Bucket   string         `json:"bucket"`
	Path     string         `json:"path"`
	AWS      *StoreAWS      `json:"aws,omitempty"`
	Azure    *StoreAzure    `json:"azure,omitempty"`
	Google   *StoreGoogle   `json:"gcp,omitempty"`
	Other    *StoreOther    `json:"other,omitempty"`
	Internal *StoreInternal `json:"internal,omitempty"`
	NFS      *StoreNFS      `json:"nfs,omitempty"`
}

type NFSConfig struct {
	Path   string `json:"path"`
	Server string `json:"server"`
}

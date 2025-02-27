// Copyright 2019 Yunion
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bingocloud

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	xj "github.com/basgys/goxml2json"

	"yunion.io/x/jsonutils"
	"yunion.io/x/log"
	"yunion.io/x/pkg/errors"

	api "yunion.io/x/onecloud/pkg/apis/compute"
	"yunion.io/x/onecloud/pkg/cloudprovider"
	"yunion.io/x/onecloud/pkg/util/httputils"
)

const (
	CLOUD_PROVIDER_BINGO_CLOUD = api.CLOUD_PROVIDER_BINGO_CLOUD

	MAX_RESULT = 20
)

type BingoCloudConfig struct {
	cpcfg     cloudprovider.ProviderConfig
	endpoint  string
	accessKey string
	secretKey string

	debug bool
}

func NewBingoCloudClientConfig(endpoint, accessKey, secretKey string) *BingoCloudConfig {
	cfg := &BingoCloudConfig{
		endpoint:  endpoint,
		accessKey: accessKey,
		secretKey: secretKey,
	}
	return cfg
}

func (cfg *BingoCloudConfig) CloudproviderConfig(cpcfg cloudprovider.ProviderConfig) *BingoCloudConfig {
	cfg.cpcfg = cpcfg
	return cfg
}

func (cfg *BingoCloudConfig) Debug(debug bool) *BingoCloudConfig {
	cfg.debug = debug
	return cfg
}

type SBingoCloudClient struct {
	*BingoCloudConfig

	regions []SRegion
}

func NewBingoCloudClient(cfg *BingoCloudConfig) (*SBingoCloudClient, error) {
	client := &SBingoCloudClient{BingoCloudConfig: cfg}
	var err error
	client.regions, err = client.GetRegions()
	if err != nil {
		return nil, err
	}
	for i := range client.regions {
		client.regions[i].client = client
	}
	return client, nil
}

func (self *SBingoCloudClient) GetAccountId() string {
	return self.endpoint
}

func (self *SBingoCloudClient) GetRegion(id string) (*SRegion, error) {
	for i := range self.regions {
		if self.regions[i].RegionId == id {
			return &self.regions[i], nil
		}
	}
	if len(id) == 0 {
		return &self.regions[0], nil
	}
	return nil, cloudprovider.ErrNotFound
}

func (cli *SBingoCloudClient) getDefaultClient(timeout time.Duration) *http.Client {
	client := httputils.GetDefaultClient()
	if timeout > 0 {
		client = httputils.GetTimeoutClient(timeout)
	}
	if cli.cpcfg.ProxyFunc != nil {
		httputils.SetClientProxyFunc(client, cli.cpcfg.ProxyFunc)
	}
	return client
}

func (self *SBingoCloudClient) sign(query string) string {
	uri, _ := url.Parse(self.endpoint)
	items := strings.Split(query, "&")
	sort.Slice(items, func(i, j int) bool {
		x0, y0 := strings.Split(items[i], "=")[0], strings.Split(items[j], "=")[0]
		return x0 < y0
	})
	path := "/"
	if len(uri.Path) > 0 {
		path = uri.Path
	}
	stringToSign := fmt.Sprintf("POST\n%s\n%s\n", uri.Host, path) + strings.Join(items, "&")
	hmac := hmac.New(sha256.New, []byte(self.secretKey))
	hmac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(hmac.Sum(nil))
}

func setItemToArray(obj jsonutils.JSONObject) jsonutils.JSONObject {
	objDict, ok := obj.(*jsonutils.JSONDict)
	if ok {
		for k, v := range objDict.Value() {
			if v.String() == `""` {
				objDict.Remove(k)
				continue
			}
			vDict, ok := v.(*jsonutils.JSONDict)
			if ok {
				if vDict.Contains("item") {
					item, _ := vDict.Get("item")
					_, ok := item.(*jsonutils.JSONArray)
					if !ok {
						if k != "instancesSet" {
							item = setItemToArray(item)
							objDict.Set(k, jsonutils.NewArray(item))
						} else {
							objDict.Set(k, setItemToArray(item))
						}
					} else {
						items, _ := item.GetArray()
						for i := range items {
							items[i] = setItemToArray(items[i])
						}
						objDict.Set(k, jsonutils.NewArray(items...))
					}
					for _, nk := range []string{"nextToken", "NextToken"} {
						nextToken, _ := vDict.GetString(nk)
						if len(nextToken) > 0 {
							objDict.Set(nk, jsonutils.NewString(nextToken))
						}
					}
				} else {
					objDict.Set(k, setItemToArray(v))
				}
			} else if _, ok = v.(*jsonutils.JSONArray); ok {
				if ok {
					arr, _ := v.GetArray()
					for i := range arr {
						arr[i] = setItemToArray(arr[i])
					}
					objDict.Set(k, jsonutils.NewArray(arr...))
				}
			}
		}
	}
	_, ok = obj.(*jsonutils.JSONArray)
	if ok {
		arr, _ := obj.GetArray()
		for i := range arr {
			arr[i] = setItemToArray(arr[i])
		}
		return jsonutils.NewArray(arr...)
	}
	return objDict
}

type sBingoError struct {
	Response struct {
		Errors struct {
			Error struct {
				Code    string
				ErrorNo string
				Message string
			}
		}
	}
}

func (e sBingoError) Error() string {
	return jsonutils.Marshal(e.Response.Errors.Error).String()
}

func (self *SBingoCloudClient) invoke(action string, params map[string]string) (jsonutils.JSONObject, error) {
	if self.cpcfg.ReadOnly {
		for _, prefix := range []string{"Get", "List", "Describe"} {
			if strings.HasPrefix(action, prefix) {
				return nil, errors.Wrapf(cloudprovider.ErrAccountReadOnly, action)
			}
		}
	}
	var encode = func(k, v string) string {
		d := url.Values{}
		d.Set(k, v)
		return d.Encode()
	}
	query := encode("Action", action)
	for k, v := range params {
		query += "&" + encode(k, v)
	}
	// 2022-02-11T03:57:37.000Z
	sh, _ := time.LoadLocation("Asia/Shanghai")
	timeStamp := time.Now().In(sh).Format("2006-01-02T15:04:05.000Z")
	query += "&" + encode("Timestamp", timeStamp)
	query += "&" + encode("AWSAccessKeyId", self.accessKey)
	query += "&" + encode("Version", "2009-08-15")
	query += "&" + encode("SignatureVersion", "2")
	query += "&" + encode("SignatureMethod", "HmacSHA256")
	query += "&" + encode("Signature", self.sign(query))
	client := self.getDefaultClient(0)
	resp, err := httputils.Request(client, context.Background(), httputils.POST, self.endpoint, nil, strings.NewReader(query), self.debug)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	data, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	result, err := xj.Convert(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}

	obj, err := jsonutils.Parse([]byte(result.String()))
	if err != nil {
		return nil, errors.Wrapf(err, "jsonutils.Parse")
	}

	obj = setItemToArray(obj)

	if self.debug {
		log.Debugf("response: %s", obj.PrettyString())
	}

	be := &sBingoError{}
	obj.Unmarshal(be)
	if len(be.Response.Errors.Error.Code) > 0 {
		return nil, be
	}

	respKey := action + "Response"
	if obj.Contains(respKey) {
		obj, err = obj.Get(respKey)
		if err != nil {
			return nil, err
		}
	}

	return obj, nil
}

func (self *SBingoCloudClient) GetSubAccounts() ([]cloudprovider.SSubAccount, error) {
	subAccount := cloudprovider.SSubAccount{
		Account: self.accessKey,
		Name:    self.cpcfg.Name,

		HealthStatus: api.CLOUD_PROVIDER_HEALTH_NORMAL,
	}
	return []cloudprovider.SSubAccount{subAccount}, nil

}

func (self *SBingoCloudClient) GetIRegions() []cloudprovider.ICloudRegion {
	ret := []cloudprovider.ICloudRegion{}
	for i := range self.regions {
		self.regions[i].client = self
		ret = append(ret, &self.regions[i])
	}
	return ret
}

func (self *SBingoCloudClient) GetIRegionById(id string) (cloudprovider.ICloudRegion, error) {
	iregions := self.GetIRegions()
	for i := range iregions {
		if iregions[i].GetGlobalId() == id {
			return iregions[i], nil
		}
	}
	return nil, errors.Wrapf(cloudprovider.ErrNotFound, id)
}

func (self *SBingoCloudClient) GetCapabilities() []string {
	return []string{
		cloudprovider.CLOUD_CAPABILITY_COMPUTE + cloudprovider.READ_ONLY_SUFFIX,
		cloudprovider.CLOUD_CAPABILITY_NETWORK + cloudprovider.READ_ONLY_SUFFIX,
		cloudprovider.CLOUD_CAPABILITY_EIP + cloudprovider.READ_ONLY_SUFFIX,
	}
}

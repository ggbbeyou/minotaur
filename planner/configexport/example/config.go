// Code generated by minotaur-config-export. DO NOT EDIT.
package example
import (
	jsonIter "github.com/json-iterator/go"
	"github.com/kercylan98/minotaur/utils/log"
	"go.uber.org/zap"
	"os"
)

var json = jsonIter.ConfigCompatibleWithStandardLibrary
var (
	 IIndexConfig map[int]map[string]*IndexConfig
	 iIndexConfig map[int]map[string]*IndexConfig
	 IEasyConfig *EasyConfig
	 iEasyConfig *EasyConfig
)

func LoadConfig(handle func(filename string, config any) error) {
	var err error
	iIndexConfig = make(map[int]map[string]*IndexConfig)
	if err = handle("server.IndexConfig.json", &iIndexConfig); err != nil {
		log.Error("Config", zap.String("Name", "IndexConfig"), zap.Bool("Invalid", true), zap.Error(err))
	}

	iEasyConfig = new(EasyConfig)
	if err = handle("server.EasyConfig.json", iEasyConfig); err != nil {
		log.Error("Config", zap.String("Name", "EasyConfig"), zap.Bool("Invalid", true), zap.Error(err))
	}

}

func Refresh() {
	IIndexConfig = iIndexConfig
	IEasyConfig = iEasyConfig
}

func DefaultLoad(filepath string) {
	LoadConfig(func(filename string, config any) error {
	bytes, err := os.ReadFile(filepath)
	if err != nil {
		return err
	}

	return json.Unmarshal(bytes, &config)
	})
}


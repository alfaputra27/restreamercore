package main

import (
	"flag"
	"fmt"
	"os"
	"strings"

	"github.com/datarhei/core/v16/glob"

	"github.com/casbin/casbin/v2"
)

func main() {
	var subject string
	var domain string
	var object string
	var action string

	flag.StringVar(&subject, "subject", "", "subject of this request")
	flag.StringVar(&domain, "domain", "$none", "domain of this request")
	flag.StringVar(&object, "object", "", "object of this request")
	flag.StringVar(&action, "action", "", "action of this request")

	flag.Parse()

	e, err := casbin.NewEnforcer("./model.conf", "./policy.csv")
	if err != nil {
		fmt.Printf("error: %s\n", err)
		os.Exit(1)
	}

	e.AddFunction("ResourceMatch", ResourceMatchFunc)
	e.AddFunction("ActionMatch", ActionMatchFunc)

	ok, err := e.Enforce(subject, domain, object, action)
	if err != nil {
		fmt.Printf("error: %s\n", err)
		os.Exit(1)
	}

	if ok {
		fmt.Printf("OK\n")
	} else {
		fmt.Printf("not OK\n")
	}
}

func ResourceMatch(request, domain, policy string) bool {
	reqPrefix, reqResource := getPrefix(request)
	polPrefix, polResource := getPrefix(policy)

	if reqPrefix != polPrefix {
		return false
	}

	fmt.Printf("prefix: %s\n", reqPrefix)
	fmt.Printf("requested resource: %s\n", reqResource)
	fmt.Printf("requested domain: %s\n", domain)
	fmt.Printf("policy resource: %s\n", polResource)

	var match bool
	var err error

	if reqPrefix == "processid" {
		match, err = glob.Match(polResource, reqResource)
		if err != nil {
			return false
		}
	} else if reqPrefix == "api" {
		match, err = glob.Match(polResource, reqResource, rune('/'))
		if err != nil {
			return false
		}
	} else if reqPrefix == "fs" {
		match, err = glob.Match(polResource, reqResource, rune('/'))
		if err != nil {
			return false
		}
	} else if reqPrefix == "rtmp" {
		match, err = glob.Match(polResource, reqResource)
		if err != nil {
			return false
		}
	} else if reqPrefix == "srt" {
		match, err = glob.Match(polResource, reqResource)
		if err != nil {
			return false
		}
	}

	fmt.Printf("match: %v\n", match)

	return match
}

func ResourceMatchFunc(args ...interface{}) (interface{}, error) {
	name1 := args[0].(string)
	name2 := args[1].(string)
	name3 := args[2].(string)

	return (bool)(ResourceMatch(name1, name2, name3)), nil
}

func ActionMatch(request string, policy string) bool {
	request = strings.ToUpper(request)
	actions := strings.Split(strings.ToUpper(policy), "|")
	if len(actions) == 0 {
		return false
	}

	for _, a := range actions {
		if request == a {
			return true
		}
	}

	return false
}

func ActionMatchFunc(args ...interface{}) (interface{}, error) {
	name1 := args[0].(string)
	name2 := args[1].(string)

	return (bool)(ActionMatch(name1, name2)), nil
}

func getPrefix(s string) (string, string) {
	splits := strings.SplitN(s, ":", 2)

	if len(splits) == 0 {
		return "", ""
	}

	if len(splits) == 1 {
		return "", splits[0]
	}

	return splits[0], splits[1]
}

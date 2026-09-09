package reviewbot

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReviewWorkflowThroughRealACPSubprocesses(t *testing.T) {
	e, s, f, _, _ := fixture(t)
	script := filepath.Join(canonicalTestDir(t), "agent.py")
	source := `import json, sys

def send(value):
    print(json.dumps(value), flush=True)

for line in sys.stdin:
    request=json.loads(line)
    method=request.get('method')
    if 'id' not in request:
        continue
    if method=='initialize':
        result={'protocolVersion':1,'agentCapabilities':{},'authMethods':[]}
    elif method=='session/new':
        result={'sessionId':'fixture'}
    elif method=='session/prompt':
        prompt=''.join(block.get('text','') for block in request['params']['prompt'])
        if 'REVIEW_VERIFY {' in prompt:
            data=json.loads(prompt.split('Context (data):\n')[1])
            text='REVIEW_VERIFY '+json.dumps({'verdict':'confirmed','reason':'Independently checked changed division and discussion','checked':[d['id'] for d in data['Discussion']]})
        else:
            text='REVIEW_RESULT '+json.dumps({'summary':'Read calc.go; verified the zero-input source path','findings':[{'severity':'P1','title':'Zero input divides by zero','explanation':'The changed division panics for zero','trigger':'value(0)','evidence':['10 / n divides by zero when n is zero'],'path':'calc.go','line':3,'side':'RIGHT'}]})
        send({'jsonrpc':'2.0','method':'session/update','params':{'sessionId':'fixture','update':{'sessionUpdate':'agent_message_chunk','content':{'type':'text','text':text}}}})
        result={'stopReason':'end_turn'}
    else:
        send({'jsonrpc':'2.0','id':request['id'],'error':{'code':-32601,'message':'unsupported fixture method'}})
        continue
    send({'jsonrpc':'2.0','id':request['id'],'result':result})
`
	if err := os.WriteFile(script, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	e.config.Agent.Command = []string{"python3", script}
	e.agent = func(c Config) Agent { return agentProcess{c, e.log} }
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if err := e.step(ctx, s); err != nil {
		t.Fatal(err)
	}
	if f.creates != 1 || len(s.Jobs[0].Payload.Comments) != 1 {
		t.Fatal("ACP review did not reach simulated publisher")
	}
	sessions, err := os.ReadDir(filepath.Join(e.config.StateDirectory, "sessions"))
	if err != nil || len(sessions) != 2 {
		t.Fatalf("expected separate private transcripts: %v %v", sessions, err)
	}
	for _, session := range sessions {
		info, err := session.Info()
		if err != nil || info.Mode().Perm() != 0600 {
			t.Fatal("transcript permissions", err)
		}
	}
}

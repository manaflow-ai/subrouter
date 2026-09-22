package proxy

import (
 "context"
 "encoding/json"
 "errors"
 "io"
 "net/http"
 "os"
 "os/exec"
 "path/filepath"
 "strings"
 "sync"
 "sync/atomic"
 "testing"
 "time"
)

func testBudget(t *testing.T, limit int64) *bedrockBudgetGuard {
 t.Helper()
 path := filepath.Join(t.TempDir(), "budget.json")
 state := bedrockBudgetState{Version:2, Account:"123456789012", Limit:limit, Spent:0, Pending:map[string]int64{}, Policies:map[string]bedrockBudgetPolicy{
 bedrockFableModelID:{MaxInput:1000000, MaxOutput:128000, InputMicros:20, OutputMicros:50, Expires:time.Now().Add(time.Hour)},
 }}
 b,_ := json.Marshal(state)
 if err:=os.WriteFile(path,b,0600);err!=nil {t.Fatal(err)}
 g,err:=NewBedrockBudgetGuard(path,state.Account,float64(limit)/1e6);if err!=nil {t.Fatal(err)}
 return g
}
func budgetReserve(g *bedrockBudgetGuard) (*bedrockBudgetReservation,error) {
 return g.reserve("POST", "/model/"+bedrockFableModelID+"/invoke", "", http.Header{}, []byte(`{"max_tokens":100}`))
}
func TestBedrockBudgetDurableAcrossWorkersAndCrash(t *testing.T) {
 g:=testBudget(t,30000000)
 if _,err:=budgetReserve(g);err!=nil {t.Fatal(err)}
 // Another worker, or a restart after a crash, must see the pending charge.
 g2,err:=NewBedrockBudgetGuard(g.path,g.account,30);if err!=nil {t.Fatal(err)}
 if _,err:=budgetReserve(g2);err==nil {t.Fatal("lost durable reservation")}
}
func TestBedrockBudgetConcurrentWorkers(t *testing.T) {
 g:=testBudget(t,60015000) // exactly three full reservations
 var wg sync.WaitGroup; var admitted atomic.Int32
 for i:=0;i<20;i++ {wg.Add(1);go func(){defer wg.Done();other,err:=NewBedrockBudgetGuard(g.path,g.account,60.015);if err!=nil {t.Error(err);return};if _,err=budgetReserve(other);err==nil{admitted.Add(1)}}()};wg.Wait()
 if admitted.Load()!=3 {t.Fatalf("admitted %d, want 3",admitted.Load())}
}
func TestBedrockBudgetMissingCorruptChangedState(t *testing.T) {
 for _,bad:=range []string{"",`{}`,`{"version":1,"spent_usd":0}`,`{"version":2,"limit_microusd":-1}`} {t.Run(bad,func(t *testing.T){g:=testBudget(t,30000000);if bad==""{os.Remove(g.path)}else{os.WriteFile(g.path,[]byte(bad),0600)};if _,err:=budgetReserve(g);err==nil {t.Fatal("bad state admitted request")}})}
 g:=testBudget(t,30000000)
 if _,err:=NewBedrockBudgetGuard(g.path,"000000000000",30);err==nil{t.Fatal("account mismatch accepted")}
 if _,err:=NewBedrockBudgetGuard(g.path,g.account,31);err==nil{t.Fatal("limit change accepted")}
}
func TestBedrockBudgetRejectsUnpricedSurfaces(t *testing.T) {
 g:=testBudget(t,30000000)
 for _,path:=range []string{"/async-invoke","/model/unknown/invoke","/model/"+bedrockFableModelID+"/converse","/model/"+bedrockFableModelID+"/invoke/"} {
 if _,err:=g.reserve("POST",path,"",nil,[]byte(`{"max_tokens":1}`));err==nil {t.Errorf("accepted %s",path)}
 }
 for _,body:=range []string{`{}`,`{"max_tokens":-1}`,`{"max_tokens":999999999}`,`{"max_tokens":1,"service_tier":"priority"}`,`{"max_tokens":1,"max_tokens":2}`} {
 if _,err:=g.reserve("POST","/model/"+bedrockFableModelID+"/invoke","",nil,[]byte(body));err==nil {t.Errorf("accepted %s",body)}
 }
}
func TestBedrockBudgetRetriesDebitSeparately(t *testing.T) {
 g:=testBudget(t,30000000); var calls int
 cfg:=&BedrockConfig{Budget:g,Regions:[]string{"us-east-1","us-west-2"},Sources:[]BedrockCredentialSource{{Name:"test",AccountID:g.account,Credentials:staticBedrockCreds()}},Transport:bedrockRoundTripFunc(func(*http.Request)(*http.Response,error){calls++;return nil,errors.New("unknown upstream outcome")})}
 s:=Server{Bedrock:cfg}
 _,_,_,_ =s.signAndForwardBedrock(context.Background(),0,"POST","/model/"+bedrockFableModelID+"/invoke",[]byte(`{"max_tokens":100}`))
 if calls!=1 {t.Fatalf("forwarded %d attempts, want 1",calls)}
}
func TestBedrockBudgetCompleteUsageOnly(t *testing.T) {
 g:=testBudget(t,30000000)
 r,err:=budgetReserve(g);if err!=nil{t.Fatal(err)}
 body:=budgetResponseBody(io.NopCloser(strings.NewReader(`{"stop_reason":"end_turn","usage":{"input_tokens":100,"output_tokens":2}}`)),r,false,true)
 if _,err=io.ReadAll(body);err!=nil{t.Fatal(err)};body.Close()
 state,err:=g.snapshot();if err!=nil{t.Fatal(err)}
 if state.Spent!=2100 || len(state.Pending)!=0 {t.Fatalf("bad settlement: %+v",state)}
 r,err=budgetReserve(g);if err!=nil{t.Fatal(err)}
 body=budgetResponseBody(io.NopCloser(strings.NewReader(`{"usage":{"input_tokens":100}}`)),r,false,true)
 io.Copy(io.Discard,body);body.Close()
 state,_=g.snapshot();if len(state.Pending)!=1 {t.Fatal("incomplete usage released reservation")}
}
func TestBedrockBudgetSeparateProcess(t *testing.T) {
 if os.Getenv("BEDROCK_BUDGET_TEST_CHILD")=="1" {
 g,err:=NewBedrockBudgetGuard(os.Getenv("BEDROCK_BUDGET_TEST_PATH"),"123456789012",30);if err!=nil{t.Fatal(err)}
 if _,err=budgetReserve(g);err==nil {t.Fatal("other process ignored pending reservation")};return
 }
 g:=testBudget(t,30000000);if _,err:=budgetReserve(g);err!=nil{t.Fatal(err)}
 cmd:=exec.Command(os.Args[0],"-test.run=^TestBedrockBudgetSeparateProcess$")
 cmd.Env=append(os.Environ(),"BEDROCK_BUDGET_TEST_CHILD=1","BEDROCK_BUDGET_TEST_PATH="+g.path)
 if b,err:=cmd.CombinedOutput();err!=nil{t.Fatalf("child failed: %v %s",err,b)}
}

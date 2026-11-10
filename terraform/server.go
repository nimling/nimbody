package terraform

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"

	"github.com/hashicorp/terraform-plugin-go/tfprotov6"
	"github.com/hashicorp/terraform-plugin-go/tftypes"
	"github.com/nimling/nimbody/cli"
	"github.com/nimling/nimbody/state"
)

type server struct {
	provider  Provider
	mu        sync.Mutex
	api       *client
	document  *state.Document
	resources map[string]resource
	problems  []*tfprotov6.Diagnostic
}

func failed(summary string, err error) []*tfprotov6.Diagnostic {
	return []*tfprotov6.Diagnostic{{Severity: tfprotov6.DiagnosticSeverityError, Summary: summary, Detail: err.Error()}}
}

func (p Provider) env(key string) string {
	if p.Client == nil || p.Client.Settings == nil {
		return strings.ToUpper(key)
	}
	for _, setting := range p.Client.Settings.Keys {
		if setting.Key == key && setting.Env != "" {
			return setting.Env
		}
	}
	return p.Client.Settings.Prefix + strings.ToUpper(key)
}

func (p Provider) schema() *tfprotov6.Schema {
	return &tfprotov6.Schema{Block: &tfprotov6.SchemaBlock{Description: "The " + p.Name + " service, reached at its endpoint with a machine client or a token", Attributes: []*tfprotov6.SchemaAttribute{
		{Name: cli.KeyAppSlug, Type: tftypes.String, Optional: true, Description: "the app slug sent with every request, or " + p.env(cli.KeyAppSlug)},
		{Name: cli.KeyClientID, Type: tftypes.String, Optional: true, Description: "the machine client id, or " + p.env(cli.KeyClientID)},
		{Name: cli.KeyClientSecret, Type: tftypes.String, Optional: true, Sensitive: true, Description: "the machine client secret, or " + p.env(cli.KeyClientSecret)},
		{Name: cli.KeyEndpoint, Type: tftypes.String, Optional: true, Description: "the service base url, or " + p.env(cli.KeyEndpoint)},
		{Name: cli.KeyToken, Type: tftypes.String, Optional: true, Sensitive: true, Description: "a bearer token, used before the client id and secret, or " + p.env(cli.KeyToken)},
	}}}
}

func (s *server) load() (bool, []*tfprotov6.Diagnostic) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.document != nil {
		return true, s.problems
	}
	doc, err := wire(s.provider.Document)
	if err != nil {
		return false, failed("the "+s.provider.Name+" state schema cannot be read", err)
	}
	if doc.Format != state.FormatVersion {
		return false, failed("the "+s.provider.Name+" state format does not match", errors.New("the schema in this provider is state format "+doc.Format+" and this provider reads "+state.FormatVersion+"; build the provider from the state definitions of its service"))
	}
	s.document = &doc
	s.resources = map[string]resource{}
	for _, group := range doc.Groups {
		r, err := resourceOf(group)
		if err != nil {
			s.problems = append(s.problems, &tfprotov6.Diagnostic{Severity: tfprotov6.DiagnosticSeverityWarning, Summary: "group " + group.Name + " is not served", Detail: err.Error()})
			continue
		}
		s.resources[s.provider.Name+"_"+group.Name] = r
	}
	return true, s.problems
}

func wire(doc state.Document) (state.Document, error) {
	var out state.Document
	body, err := json.Marshal(doc)
	if err != nil {
		return out, err
	}
	return out, json.Unmarshal(body, &out)
}

func (s *server) resource(name string) (resource, state.Document, []*tfprotov6.Diagnostic) {
	if loaded, problems := s.load(); !loaded {
		return resource{}, state.Document{}, problems
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.resources[name]
	if !ok {
		return resource{}, state.Document{}, failed("no resource "+name, errors.New("the provider has no state group for "+name))
	}
	return r, *s.document, nil
}

func (s *server) remote(name string) (resource, client, state.Document, []*tfprotov6.Diagnostic) {
	r, doc, problems := s.resource(name)
	if problems != nil {
		return r, client{}, doc, problems
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.api == nil {
		return r, client{}, doc, failed("the "+s.provider.Name+" provider is not configured", errors.New("terraform calls the service only after it configured the provider"))
	}
	return r, *s.api, doc, nil
}

func (s *server) GetMetadata(ctx context.Context, req *tfprotov6.GetMetadataRequest) (*tfprotov6.GetMetadataResponse, error) {
	resp := &tfprotov6.GetMetadataResponse{ServerCapabilities: &tfprotov6.ServerCapabilities{}}
	_, resp.Diagnostics = s.load()
	for _, name := range s.names() {
		resp.Resources = append(resp.Resources, tfprotov6.ResourceMetadata{TypeName: name})
	}
	return resp, nil
}

func (s *server) names() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	names := make([]string, 0, len(s.resources))
	for name := range s.resources {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (s *server) GetProviderSchema(ctx context.Context, req *tfprotov6.GetProviderSchemaRequest) (*tfprotov6.GetProviderSchemaResponse, error) {
	resp := &tfprotov6.GetProviderSchemaResponse{
		ServerCapabilities: &tfprotov6.ServerCapabilities{},
		Provider:           s.provider.schema(),
		ResourceSchemas:    map[string]*tfprotov6.Schema{},
		DataSourceSchemas:  map[string]*tfprotov6.Schema{},
		Functions:          map[string]*tfprotov6.Function{},
	}
	_, resp.Diagnostics = s.load()
	s.mu.Lock()
	defer s.mu.Unlock()
	for name, r := range s.resources {
		resp.ResourceSchemas[name] = r.schema
	}
	return resp, nil
}

func (s *server) GetResourceIdentitySchemas(ctx context.Context, req *tfprotov6.GetResourceIdentitySchemasRequest) (*tfprotov6.GetResourceIdentitySchemasResponse, error) {
	return &tfprotov6.GetResourceIdentitySchemasResponse{IdentitySchemas: map[string]*tfprotov6.ResourceIdentitySchema{}}, nil
}

func (s *server) ValidateProviderConfig(ctx context.Context, req *tfprotov6.ValidateProviderConfigRequest) (*tfprotov6.ValidateProviderConfigResponse, error) {
	return &tfprotov6.ValidateProviderConfigResponse{PreparedConfig: req.Config}, nil
}

func (s *server) ConfigureProvider(ctx context.Context, req *tfprotov6.ConfigureProviderRequest) (*tfprotov6.ConfigureProviderResponse, error) {
	resp := &tfprotov6.ConfigureProviderResponse{}
	loaded, problems := s.load()
	if resp.Diagnostics = problems; !loaded {
		return resp, nil
	}
	if s.provider.Client == nil || s.provider.Client.Settings == nil {
		resp.Diagnostics = failed("the "+s.provider.Name+" provider has no client", errors.New("terraform.Provider needs the cli's Client"))
		return resp, nil
	}
	base := s.provider.Client
	settings := &cli.Settings{Name: base.Settings.Name, Prefix: base.Settings.Prefix, Keys: base.Settings.Keys}
	if req.Config != nil {
		config, err := req.Config.Unmarshal(s.provider.schema().ValueType())
		if err != nil {
			resp.Diagnostics = failed("the provider config cannot be read", err)
			return resp, nil
		}
		for _, attribute := range s.provider.schema().Block.Attributes {
			value, _, err := jsonOf(at(config, tftypes.NewAttributePath().WithAttributeName(attribute.Name), tftypes.String))
			if err != nil {
				resp.Diagnostics = failed("the provider config cannot be read", err)
				return resp, nil
			}
			if text, _ := value.(string); text != "" {
				settings.Put(attribute.Name, text)
			}
		}
	}
	api := &cli.Client{Settings: settings, Product: base.Product, Version: base.Version, LoginPath: base.LoginPath, TokenPath: base.TokenPath, RevokePath: base.RevokePath, IdentityPath: base.IdentityPath, Exchange: base.Exchange, HealthPath: base.HealthPath, AppHeader: base.AppHeader, HTTP: base.HTTP}
	if api.Endpoint() == "" {
		resp.Diagnostics = append(resp.Diagnostics, failed("the "+s.provider.Name+" provider has no endpoint", errors.New("set endpoint in the provider block or "+s.provider.env(cli.KeyEndpoint)))...)
		return resp, nil
	}
	s.mu.Lock()
	s.api = &client{api: api}
	s.mu.Unlock()
	return resp, nil
}

func (s *server) StopProvider(ctx context.Context, req *tfprotov6.StopProviderRequest) (*tfprotov6.StopProviderResponse, error) {
	return &tfprotov6.StopProviderResponse{}, nil
}

func (s *server) ValidateResourceConfig(ctx context.Context, req *tfprotov6.ValidateResourceConfigRequest) (*tfprotov6.ValidateResourceConfigResponse, error) {
	_, _, problems := s.resource(req.TypeName)
	return &tfprotov6.ValidateResourceConfigResponse{Diagnostics: problems}, nil
}

func (s *server) UpgradeResourceState(ctx context.Context, req *tfprotov6.UpgradeResourceStateRequest) (*tfprotov6.UpgradeResourceStateResponse, error) {
	resp := &tfprotov6.UpgradeResourceStateResponse{}
	r, _, problems := s.resource(req.TypeName)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	value, err := req.RawState.UnmarshalWithOpts(r.typ, tfprotov6.UnmarshalOpts{ValueFromJSONOpts: tftypes.ValueFromJSONOpts{IgnoreUndefinedAttributes: true}})
	if err != nil {
		resp.Diagnostics = failed("the stored state of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	resp.UpgradedState, resp.Diagnostics = dynamic(r.typ, value)
	return resp, nil
}

func dynamic(typ tftypes.Type, value tftypes.Value) (*tfprotov6.DynamicValue, []*tfprotov6.Diagnostic) {
	out, err := tfprotov6.NewDynamicValue(typ, value)
	if err != nil {
		return nil, failed("a value cannot be encoded", err)
	}
	return &out, nil
}

func decoded(value *tfprotov6.DynamicValue, typ tftypes.Type) (tftypes.Value, error) {
	if value == nil {
		return tftypes.NewValue(typ, nil), nil
	}
	return value.Unmarshal(typ)
}

func keyOf(value tftypes.Value) string {
	key, _, _ := jsonOf(at(value, tftypes.NewAttributePath().WithAttributeName(KeyAttribute), tftypes.String))
	text, _ := key.(string)
	return text
}

func (s *server) ReadResource(ctx context.Context, req *tfprotov6.ReadResourceRequest) (*tfprotov6.ReadResourceResponse, error) {
	resp := &tfprotov6.ReadResourceResponse{}
	r, api, _, problems := s.remote(req.TypeName)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	current, err := decoded(req.CurrentState, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the state of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	line, found, err := api.GetRecord(ctx, r.group.Name, keyOf(current))
	if err != nil {
		resp.Diagnostics = failed("export of "+r.group.Name+" "+keyOf(current)+" failed", err)
		return resp, nil
	}
	if !found {
		resp.NewState, resp.Diagnostics = dynamic(r.typ, tftypes.NewValue(r.typ, nil))
		return resp, nil
	}
	value, err := r.value(line)
	if err != nil {
		resp.Diagnostics = failed(r.group.Name+" "+line.Key+" does not fit its schema", err)
		return resp, nil
	}
	resp.NewState, resp.Diagnostics = dynamic(r.typ, value)
	resp.Private = req.Private
	return resp, nil
}

func (s *server) PlanResourceChange(ctx context.Context, req *tfprotov6.PlanResourceChangeRequest) (*tfprotov6.PlanResourceChangeResponse, error) {
	resp := &tfprotov6.PlanResourceChangeResponse{}
	r, api, doc, problems := s.remote(req.TypeName)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	proposed, err := decoded(req.ProposedNewState, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the plan of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	if proposed.IsNull() {
		resp.PlannedState = req.ProposedNewState
		return resp, nil
	}
	prior, err := decoded(req.PriorState, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the state of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	config, err := decoded(req.Config, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the config of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	planned, err := tftypes.Transform(proposed, func(path *tftypes.AttributePath, value tftypes.Value) (tftypes.Value, error) {
		switch {
		case matches(r.writeOnly, path):
			return tftypes.NewValue(value.Type(), nil), nil
		case matches(r.computed, path) && at(config, path, value.Type()).IsNull():
			if prior.IsNull() {
				return tftypes.NewValue(value.Type(), tftypes.UnknownValue), nil
			}
			return at(prior, path, value.Type()), nil
		}
		return value, nil
	})
	if err != nil {
		resp.Diagnostics = failed("the plan of "+req.TypeName+" cannot be made", err)
		return resp, nil
	}
	if resp.PlannedState, resp.Diagnostics = dynamic(r.typ, planned); resp.Diagnostics != nil {
		return resp, nil
	}
	if !prior.IsNull() {
		for _, path := range r.immutable {
			if !at(prior, path, tftypes.DynamicPseudoType).Equal(at(planned, path, tftypes.DynamicPseudoType)) {
				resp.RequiresReplace = append(resp.RequiresReplace, path)
			}
		}
	}
	if len(resp.RequiresReplace) > 0 || !planned.IsFullyKnown() || (!prior.IsNull() && prior.Equal(planned)) {
		return resp, nil
	}
	line, err := r.line(planned, config)
	if err != nil {
		resp.Diagnostics = failed("the plan of "+req.TypeName+" cannot be written as a record", err)
		return resp, nil
	}
	result, err := api.PostImport(ctx, doc, &line, true)
	switch {
	case err != nil:
		resp.Diagnostics = []*tfprotov6.Diagnostic{{Severity: tfprotov6.DiagnosticSeverityWarning, Summary: "the dry run of " + r.group.Name + " " + line.Key + " failed", Detail: err.Error()}}
	case result.Outcome == state.OutcomeRefused:
		resp.Diagnostics = []*tfprotov6.Diagnostic{{Severity: tfprotov6.DiagnosticSeverityWarning, Summary: "the service would refuse " + r.group.Name + " " + line.Key + " as it stands", Detail: result.Error}}
	}
	return resp, nil
}

func (s *server) ApplyResourceChange(ctx context.Context, req *tfprotov6.ApplyResourceChangeRequest) (*tfprotov6.ApplyResourceChangeResponse, error) {
	resp := &tfprotov6.ApplyResourceChangeResponse{}
	r, api, doc, problems := s.remote(req.TypeName)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	planned, err := decoded(req.PlannedState, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the plan of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	if planned.IsNull() {
		prior, err := decoded(req.PriorState, r.typ)
		if err != nil {
			resp.Diagnostics = failed("the state of "+req.TypeName+" cannot be read", err)
			return resp, nil
		}
		resp.Diagnostics = s.delete(ctx, api, doc, r, keyOf(prior))
		if resp.Diagnostics == nil {
			resp.NewState, resp.Diagnostics = dynamic(r.typ, planned)
		}
		return resp, nil
	}
	config, err := decoded(req.Config, r.typ)
	if err != nil {
		resp.Diagnostics = failed("the config of "+req.TypeName+" cannot be read", err)
		return resp, nil
	}
	line, err := r.line(planned, config)
	if err != nil {
		resp.Diagnostics = failed("the plan of "+req.TypeName+" cannot be written as a record", err)
		return resp, nil
	}
	result, err := api.PostImport(ctx, doc, &line, false)
	if err != nil {
		resp.Diagnostics = failed("import of "+r.group.Name+" "+line.Key+" failed", err)
		return resp, nil
	}
	if result.Outcome == state.OutcomeRefused {
		resp.Diagnostics = failed("the service refused "+r.group.Name+" "+line.Key, errors.New(result.Error))
		return resp, nil
	}
	written, found, err := api.GetRecord(ctx, r.group.Name, line.Key)
	if err == nil && !found {
		err = errors.New("the service answers no record for it after the import")
	}
	if err != nil {
		resp.Diagnostics = failed("export of "+r.group.Name+" "+line.Key+" failed", err)
		return resp, nil
	}
	read, err := r.value(written)
	if err != nil {
		resp.Diagnostics = failed(r.group.Name+" "+line.Key+" does not fit its schema", err)
		return resp, nil
	}
	applied, err := tftypes.Transform(planned, func(path *tftypes.AttributePath, value tftypes.Value) (tftypes.Value, error) {
		if !value.IsKnown() {
			return at(read, path, value.Type()), nil
		}
		return value, nil
	})
	if err != nil {
		resp.Diagnostics = failed("the state of "+req.TypeName+" cannot be made", err)
		return resp, nil
	}
	resp.NewState, resp.Diagnostics = dynamic(r.typ, applied)
	return resp, nil
}

func (s *server) delete(ctx context.Context, api client, doc state.Document, r resource, key string) []*tfprotov6.Diagnostic {
	result, err := api.PostImport(ctx, doc, nil, false, r.group.Name+":"+key)
	if err != nil {
		return failed("delete of "+r.group.Name+" "+key+" failed", err)
	}
	if result.Outcome != state.OutcomeRefused {
		return nil
	}
	_, found, err := api.GetRecord(ctx, r.group.Name, key)
	if err != nil {
		return failed("export of "+r.group.Name+" "+key+" failed", err)
	}
	if found {
		return failed("the service refused to delete "+r.group.Name+" "+key, errors.New(result.Error))
	}
	return nil
}

func (s *server) ImportResourceState(ctx context.Context, req *tfprotov6.ImportResourceStateRequest) (*tfprotov6.ImportResourceStateResponse, error) {
	resp := &tfprotov6.ImportResourceStateResponse{}
	r, _, problems := s.resource(req.TypeName)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	value, err := valueOf(map[string]any{KeyAttribute: req.ID}, r.typ)
	if err != nil {
		resp.Diagnostics = failed("key "+req.ID+" cannot be imported", err)
		return resp, nil
	}
	imported, problems := dynamic(r.typ, value)
	if problems != nil {
		resp.Diagnostics = problems
		return resp, nil
	}
	resp.ImportedResources = []*tfprotov6.ImportedResource{{TypeName: req.TypeName, State: imported}}
	return resp, nil
}

func unsupported(what string) []*tfprotov6.Diagnostic {
	return failed(what+" is not served", errors.New("the provider serves state groups as resources and nothing else"))
}

func (s *server) MoveResourceState(ctx context.Context, req *tfprotov6.MoveResourceStateRequest) (*tfprotov6.MoveResourceStateResponse, error) {
	return &tfprotov6.MoveResourceStateResponse{Diagnostics: unsupported("moving state")}, nil
}

func (s *server) UpgradeResourceIdentity(ctx context.Context, req *tfprotov6.UpgradeResourceIdentityRequest) (*tfprotov6.UpgradeResourceIdentityResponse, error) {
	return &tfprotov6.UpgradeResourceIdentityResponse{Diagnostics: unsupported("resource identity")}, nil
}

func (s *server) ValidateDataResourceConfig(ctx context.Context, req *tfprotov6.ValidateDataResourceConfigRequest) (*tfprotov6.ValidateDataResourceConfigResponse, error) {
	return &tfprotov6.ValidateDataResourceConfigResponse{Diagnostics: unsupported("data source " + req.TypeName)}, nil
}

func (s *server) ReadDataSource(ctx context.Context, req *tfprotov6.ReadDataSourceRequest) (*tfprotov6.ReadDataSourceResponse, error) {
	return &tfprotov6.ReadDataSourceResponse{Diagnostics: unsupported("data source " + req.TypeName)}, nil
}

func (s *server) GetFunctions(ctx context.Context, req *tfprotov6.GetFunctionsRequest) (*tfprotov6.GetFunctionsResponse, error) {
	return &tfprotov6.GetFunctionsResponse{Functions: map[string]*tfprotov6.Function{}}, nil
}

func (s *server) CallFunction(ctx context.Context, req *tfprotov6.CallFunctionRequest) (*tfprotov6.CallFunctionResponse, error) {
	return &tfprotov6.CallFunctionResponse{Error: &tfprotov6.FunctionError{Text: "function " + req.Name + " is not served"}}, nil
}

func (s *server) ValidateEphemeralResourceConfig(ctx context.Context, req *tfprotov6.ValidateEphemeralResourceConfigRequest) (*tfprotov6.ValidateEphemeralResourceConfigResponse, error) {
	return &tfprotov6.ValidateEphemeralResourceConfigResponse{Diagnostics: unsupported("ephemeral resource " + req.TypeName)}, nil
}

func (s *server) OpenEphemeralResource(ctx context.Context, req *tfprotov6.OpenEphemeralResourceRequest) (*tfprotov6.OpenEphemeralResourceResponse, error) {
	return &tfprotov6.OpenEphemeralResourceResponse{Diagnostics: unsupported("ephemeral resource " + req.TypeName)}, nil
}

func (s *server) RenewEphemeralResource(ctx context.Context, req *tfprotov6.RenewEphemeralResourceRequest) (*tfprotov6.RenewEphemeralResourceResponse, error) {
	return &tfprotov6.RenewEphemeralResourceResponse{Diagnostics: unsupported("ephemeral resource " + req.TypeName)}, nil
}

func (s *server) CloseEphemeralResource(ctx context.Context, req *tfprotov6.CloseEphemeralResourceRequest) (*tfprotov6.CloseEphemeralResourceResponse, error) {
	return &tfprotov6.CloseEphemeralResourceResponse{Diagnostics: unsupported("ephemeral resource " + req.TypeName)}, nil
}

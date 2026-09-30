package automation

import (
	"strings"
	"testing"

	"openpoet/internal/application"
)

func TestTaskCapabilitiesPublishPayloadContract(t *testing.T) {
	registry, err := application.NewProjectTaskCapabilityRegistry(application.NewProjectTaskService(automationTestDB(t), nil))
	if err != nil {
		t.Fatal(err)
	}
	api := &commandAPI{capabilities: registry}
	descriptors := map[application.CapabilityName]capabilityDescriptor{}
	for _, descriptor := range api.mergedCapabilityDescriptors(Actor{}) {
		descriptors[descriptor.Name] = descriptor
	}
	fieldsOf := func(name application.CapabilityName) map[string]PlatformPayloadField {
		t.Helper()
		descriptor, ok := descriptors[name]
		if !ok || descriptor.Payload == nil {
			t.Fatalf("%s publishes no payload contract: %+v", name, descriptor)
		}
		fields := map[string]PlatformPayloadField{}
		for _, field := range descriptor.Payload.Fields {
			if field.Description == "" {
				t.Errorf("%s field %s has no description", name, field.Name)
			}
			fields[field.Name] = field
		}
		return fields
	}

	create := fieldsOf(application.CapabilityTasksCreate)
	for _, name := range []string{"project_id", "title", "description", "priority"} {
		if _, ok := create[name]; !ok {
			t.Errorf("tasks.create contract lacks %s", name)
		}
	}
	if !create["title"].Required || create["project_id"].Required {
		t.Errorf("tasks.create required flags: title=%v project_id=%v", create["title"].Required, create["project_id"].Required)
	}
	if notes := descriptors[application.CapabilityTasksCreate].Payload.Notes; !strings.Contains(notes, "sessions.create") {
		t.Errorf("tasks.create notes do not explain the session hand-off: %q", notes)
	}

	fieldsOf(application.CapabilityTasksUpdate)
	update := descriptors[application.CapabilityTasksUpdate].Payload
	if !strings.Contains(update.Target, "project_id") || !strings.Contains(update.Target, "task_id") || !strings.Contains(update.Notes, "target_invalid") {
		t.Errorf("tasks.update contract does not state that the target needs project_id and task_id: %+v", update)
	}
	if list := fieldsOf(application.CapabilityTasksList); len(list) != 4 {
		t.Errorf("tasks.list fields = %v", list)
	}
}

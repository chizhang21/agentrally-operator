# Arally agents

Arally describes reusable agents and their independent instances. Business
conversations, agent identities and the compute carrying them have distinct lives.

## Language

**AgentDefinition**:
A reusable definition of an agent's environment, configuration and instance
defaults. A definition can have multiple published revisions and many instances.
_Avoid_: AgentRuntime, AgentTemplate

**AgentInstance**:
An independent logical agent instantiated from a definition. It can serve a short
task or remain a user's resident assistant while its execution changes.
_Avoid_: AgentClaim, AgentSession

**Revision**:
An immutable published configuration of an AgentDefinition. A running instance
uses an accepted revision until an explicit change is requested.

**Execution**:
The current compute carrying an AgentInstance. Replacing an execution does not
create a new logical instance.

**Workspace**:
The files available to an instance. An execution-private workspace and an
externally managed persistent workspace have different retention lifetimes.

**Conversation**:
Application-level interaction history and context. One AgentInstance can host
multiple conversations.

**AgentGroup**:
A group of definitions and instances sharing namespace-level resource and
security policies.

// SPDX-License-Identifier: AGPL-3.0-or-later

// The templates the Submit page's "Load example" menu fills the editor with.
// They live in their own module so web/submitexamples_test.go can read them
// and check that each one passes the server's template validation.

export type ExampleKey = 'shell' | 'parameter-space' | 'usage-limit'

export const EXAMPLES: Record<ExampleKey, string> = {
  shell: `specificationVersion: "jobtemplate-2023-09"
name: hello-world
steps:
  - name: main
    script:
      actions:
        onRun:
          command: echo
          args:
            - "Hello, world!"
`,
  'parameter-space': `specificationVersion: "jobtemplate-2023-09"
name: frame-render
steps:
  - name: render
    parameterSpace:
      taskParameterDefinitions:
        - name: Frame
          type: INT
          range: "1-10"
    script:
      actions:
        onRun:
          command: echo
          args:
            - "Rendering frame {{Task.Param.Frame}}"
`,
  'usage-limit': `specificationVersion: "jobtemplate-2023-09"
name: licensed-render
steps:
  - name: render
    # Require a free slot in a usage pool before each task is dispatched.
    # The text after "amount.worker.usagepool." is the pool NAME and must
    # match a pool registered on the Usage Pools page (here: "arnold").
    hostRequirements:
      amounts:
        - name: amount.worker.usagepool.arnold   # <-- "arnold" = pool name
          min: 1
    script:
      actions:
        onRun:
          command: echo
          args:
            - "Rendering while holding an Arnold license"
`,
}

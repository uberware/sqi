import { render } from '@testing-library/react'
import { describe, expect, it } from 'vitest'
import App from '@/App'

describe('App', () => {
  it('renders the app container element', () => {
    const { container } = render(<App />)
    expect(container.querySelector('#app')).not.toBeNull()
  })
})

import { useNavigate } from 'react-router-dom'
import { Compass } from 'lucide-react'
import { Button, EmptyState } from '../components/ui'

export function NotFoundPage() {
  const navigate = useNavigate()
  return (
    <EmptyState
      icon={<Compass className="size-5" aria-hidden />}
      title="Page not found"
      description="That console page does not exist. Pick a section from the sidebar, or head back to the dashboard."
      action={
        <Button variant="primary" onClick={() => navigate('/')}>
          Go to dashboard
        </Button>
      }
    />
  )
}

import React, { PureComponent } from 'react';
import { hot } from 'react-hot-loader';
import { connect } from 'react-redux';
import { NavModel, StoreState } from 'app/types';
import { getNavModel } from 'app/core/selectors/navModel';
import PageHeader from 'app/core/components/PageHeader/PageHeader';
import { Label } from 'app/core/components/Forms/Forms';
import { getBackendSrv } from 'app/core/services/backend_srv';
import { updateLocation } from 'app/core/actions';

export interface Props {
  navModel: NavModel;
  updateLocation: typeof updateLocation;
}

export interface State {
  inviteChecked: boolean;
  user: {
    role: string;
    email: string;
    name: string;
    userName: string;
    password: string;
  };
  invite: {
    loginOrEmail: string;
    name: string;
    role: string;
    sendEmail: boolean;
  };
}

export class AddUserPage extends PureComponent<Props, State> {
  constructor(props) {
    super(props);
    this.state = {
      inviteChecked: false,
      user: {
        role: 'Editor',
        email: '',
        name: '',
        userName: '',
        password: '',
      },
      invite: {
        loginOrEmail: '',
        name: '',
        role: 'Editor',
        sendEmail: true,
      },
    };
  }

  changeEmail = e => {
    const user = Object.assign({}, this.state.user);
    const invite = Object.assign({}, this.state.invite);
    user.email = e.target.value;
    invite.loginOrEmail = e.target.value;
    this.setState({ user });
    this.setState({ invite });
  };

  changeRole = e => {
    const user = Object.assign({}, this.state.user);
    const invite = Object.assign({}, this.state.invite);
    user.role = e.target.value;
    invite.role = e.target.value;
    this.setState({ user });
    this.setState({ invite });
  };

  changeUserName = e => {
    const user = Object.assign({}, this.state.user);
    user.userName = e.target.value;
    this.setState({ user });
  };

  changeName = e => {
    const user = Object.assign({}, this.state.user);
    user.name = e.target.value;
    this.setState({ user });
  };

  changePassword = e => {
    const user = Object.assign({}, this.state.user);
    user.password = e.target.value;
    this.setState({ user });
  };

  checkInvite = e => {
    this.setState(prevState => ({ inviteChecked: !prevState.inviteChecked }));
  };

  sendInvite() {
    const backendSrv = getBackendSrv();

    return backendSrv.post('/api/org/invites', this.state.invite).then(() => {
      this.props.updateLocation({
        path: 'org/users/',
      });
    });
  }

  createUser() {
    const backendSrv = getBackendSrv();

    return backendSrv.post('/api/admin/users', this.state.user).then(() => {
      this.props.updateLocation({
        path: 'org/users/',
      });
    });
  }

  addUser = () => {
    if (this.state.inviteChecked) {
      this.sendInvite();
    } else {
      this.createUser();
    }
  };

  render() {
    const { navModel } = this.props;
    return (
      <div>
        <PageHeader model={navModel} />
        <div className="page-container page-body">
          <h3 className="page-sub-heading">Add User</h3>
          <div className="gf-form-group">
            <div className="gf-form max-width-30">
              <Label>Email</Label>
              <input
                type="text"
                required
                className="gf-form-input"
                placeholder="email@test.com"
                value={this.state.user.email}
                onChange={this.changeEmail}
              />
            </div>
            <div className="gf-form max-width-30">
              <Label>Role</Label>
              <div className="gf-form-select-wrapper width-20">
                <select className="gf-form-input" value={this.state.user.role} onChange={this.changeRole}>
                  <option value="Viewer">Viewer</option>
                  <option value="Editor">Editor</option>
                  <option value="Admin">Admin</option>
                </select>
              </div>
            </div>
            <label className="gf-form">
              <Label>Add user by invite</Label>
              <div className="gf-form-switch">
                <input id="invite" type="checkbox" onChange={this.checkInvite} />
                <label htmlFor="invite" data-on="Yes" data-off="No" />
              </div>
            </label>
          </div>
          <div className="gf-form-group">
            <div className="gf-form max-width-30" ng-if="ctrl.create">
              <Label>Username</Label>
              <input
                type="text"
                required
                className="gf-form-input"
                placeholder="username"
                value={this.state.user.userName}
                onChange={this.changeUserName}
                readOnly={this.state.inviteChecked}
              />
            </div>
            <div className="gf-form max-width-30" ng-if="ctrl.create">
              <Label>Name</Label>
              <input
                type="text"
                className="gf-form-input"
                placeholder="name (optional)"
                value={this.state.user.name}
                onChange={this.changeName}
                readOnly={this.state.inviteChecked}
              />
            </div>
            <div className="gf-form max-width-30" ng-if="ctrl.create">
              <Label>Password</Label>
              <input
                type="password"
                className="gf-form-input"
                placeholder=""
                value={this.state.user.password}
                onChange={this.changePassword}
                readOnly={this.state.inviteChecked}
              />
            </div>
          </div>
          <div className="gf-form-button-row">
            <button type="submit" className="btn btn-success" onClick={this.addUser}>
              Add
            </button>
            <a className="btn" href="org/users">
              Back
            </a>
          </div>
        </div>
      </div>
    );
  }
}

const mapStateToProps = (state: StoreState) => ({
  navModel: getNavModel(state.navIndex, 'users'),
});

const mapDispatchToProps = {
  updateLocation,
};

export default hot(module)(connect(mapStateToProps, mapDispatchToProps)(AddUserPage));
